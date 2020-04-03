package velero

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	veleroInstallCR "github.com/openshift/managed-velero-operator/pkg/apis/managed/v1alpha2"
	storageConstants "github.com/openshift/managed-velero-operator/pkg/storage/constants"

	minterv1 "github.com/openshift/cloud-credential-operator/pkg/apis/cloudcredential/v1"
	monitoringv1 "github.com/coreos/prometheus-operator/pkg/apis/monitoring/v1"
	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	veleroInstall "github.com/vmware-tanzu/velero/pkg/install"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	endpoints "github.com/aws/aws-sdk-go/aws/endpoints"
	"github.com/go-logr/logr"
	configv1 "github.com/openshift/api/config/v1"
	"github.com/operator-framework/operator-sdk/pkg/metrics"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	awsCredsSecretIDKey          = "aws_access_key_id"     // #nosec G101
	awsCredsSecretAccessKey      = "aws_secret_access_key" // #nosec G101
	veleroImageRegistry          = "docker.io/velero"
	veleroImageRegistryCN        = "registry.docker-cn.com/velero"
	veleroImageTag               = "velero:v1.3.1"
	veleroAwsImageTag            = "velero-plugin-for-aws:v1.0.1"
	credentialsRequestName       = "velero-iam-credentials"
)

func (r *ReconcileVelero) provisionVelero(reqLogger logr.Logger, namespace string, platformStatus *configv1.PlatformStatus, instance *veleroInstallCR.VeleroInstall) (reconcile.Result, error) {
	var err error

	locationConfig := make(map[string]string)
	locationConfig["region"] = platformStatus.AWS.Region

	// Install BackupStorageLocation
	foundBsl := &velerov1.BackupStorageLocation{}
	bsl := veleroInstall.BackupStorageLocation(namespace, strings.ToLower(string(platformStatus.Type)), instance.Status.StorageBucket.Name, "", locationConfig)
	if err = r.client.Get(context.TODO(), types.NamespacedName{Namespace: namespace, Name: storageConstants.DefaultVeleroBackupStorageLocation}, foundBsl); err != nil {
		if errors.IsNotFound(err) {
			// Didn't find BackupStorageLocation
			reqLogger.Info("Creating BackupStorageLocation")
			if err := controllerutil.SetControllerReference(instance, bsl, r.scheme); err != nil {
				return reconcile.Result{}, err
			}
			if err = r.client.Create(context.TODO(), bsl); err != nil {
				return reconcile.Result{}, err
			}
		} else {
			return reconcile.Result{}, err
		}
	} else {
		// BackupStorageLocation exists, check if it's updated.
		if !reflect.DeepEqual(foundBsl.Spec, bsl.Spec) {
			// Specs aren't equal, update and fix.
			reqLogger.Info("Updating BackupStorageLocation")
			foundBsl.Spec = *bsl.Spec.DeepCopy()
			if err = r.client.Update(context.TODO(), foundBsl); err != nil {
				return reconcile.Result{}, err
			}
		}
	}

	// Install VolumeSnapshotLocation
	foundVsl := &velerov1.VolumeSnapshotLocation{}
	vsl := veleroInstall.VolumeSnapshotLocation(namespace, strings.ToLower(string(platformStatus.Type)), locationConfig)
	if err = r.client.Get(context.TODO(), types.NamespacedName{Namespace: namespace, Name: "default"}, foundVsl); err != nil {
		if errors.IsNotFound(err) {
			// Didn't find VolumeSnapshotLocation
			reqLogger.Info("Creating VolumeSnapshotLocation")
			if err := controllerutil.SetControllerReference(instance, vsl, r.scheme); err != nil {
				return reconcile.Result{}, err
			}
			if err = r.client.Create(context.TODO(), vsl); err != nil {
				return reconcile.Result{}, err
			}
		} else {
			return reconcile.Result{}, err
		}
	} else {
		// VolumeSnapshotLocation exists, check if it's updated.
		if !reflect.DeepEqual(foundVsl.Spec, vsl.Spec) {
			// Specs aren't equal, update and fix.
			reqLogger.Info("Updating VolumeSnapshotLocation")
			foundVsl.Spec = *vsl.Spec.DeepCopy()
			if err = r.client.Update(context.TODO(), foundVsl); err != nil {
				return reconcile.Result{}, err
			}
		}
	}

	// Install CredentialsRequest
	partition, ok := endpoints.PartitionForRegion(endpoints.DefaultPartitions(), locationConfig["region"])
	if !ok {
		return reconcile.Result{}, fmt.Errorf("no partition found for region %q", locationConfig["region"])
	}
	foundCr := &minterv1.CredentialsRequest{}
	cr := credentialsRequest(namespace, credentialsRequestName, partition.ID(), instance.Status.StorageBucket.Name)
	if err = r.client.Get(context.TODO(), types.NamespacedName{Namespace: namespace, Name: credentialsRequestName}, foundCr); err != nil {
		if errors.IsNotFound(err) {
			// Didn't find CredentialsRequest
			reqLogger.Info("Creating CredentialsRequest")
			if err := controllerutil.SetControllerReference(instance, cr, r.scheme); err != nil {
				return reconcile.Result{}, err
			}
			if err = r.client.Create(context.TODO(), cr); err != nil {
				return reconcile.Result{}, err
			}
		} else {
			return reconcile.Result{}, err
		}
	} else {
		// CredentialsRequest exists, check if it's updated.
		if !reflect.DeepEqual(foundCr.Spec, cr.Spec) {
			// Specs aren't equal, update and fix.
			reqLogger.Info("Updating CredentialsRequest")
			foundCr.Spec = *cr.Spec.DeepCopy()
			if err = r.client.Update(context.TODO(), foundCr); err != nil {
				return reconcile.Result{}, err
			}
		}
	}

	// Install Deployment
	foundDeployment := &appsv1.Deployment{}
	deployment := veleroDeployment(namespace, determineVeleroImageRegistry(locationConfig["region"]))
	if err = r.client.Get(context.TODO(), types.NamespacedName{Namespace: namespace, Name: "velero"}, foundDeployment); err != nil {
		if errors.IsNotFound(err) {
			// Didn't find Deployment
			reqLogger.Info("Creating Deployment")
			if err := controllerutil.SetControllerReference(instance, deployment, r.scheme); err != nil {
				return reconcile.Result{}, err
			}
			if err = r.client.Create(context.TODO(), deployment); err != nil {
				return reconcile.Result{}, err
			}
		} else {
			return reconcile.Result{}, err
		}
	} else {
		// Deployment exists, check if it's updated.
		if !reflect.DeepEqual(foundDeployment.Spec, deployment.Spec) {
			// Specs aren't equal, update and fix.
			reqLogger.Info("Updating Deployment", "foundDeployment.Spec", foundDeployment.Spec, "deployment.Spec", deployment.Spec)
			foundDeployment.Spec = *deployment.Spec.DeepCopy()
			if err = r.client.Update(context.TODO(), foundDeployment); err != nil {
				return reconcile.Result{}, err
			}
		}
	}

	// Install Metrics Service
	foundService := &corev1.Service{}
	service := metricsServiceFromDeployment(deployment)
	serviceName := types.NamespacedName{
		Namespace: service.ObjectMeta.Namespace,
		Name:      service.ObjectMeta.Name,
	}
	if err = r.client.Get(context.TODO(), serviceName, foundService); err != nil {
		if errors.IsNotFound(err) {
			// Didn't find Service
			reqLogger.Info("Creating Service")
			if err := controllerutil.SetControllerReference(instance, service, r.scheme); err != nil {
				return reconcile.Result{}, err
			}
			if err = r.client.Create(context.TODO(), service); err != nil {
				return reconcile.Result{}, err
			}
			// We need a populated foundService (with a UID) to generate the
			// ServiceMonitor below, so requeue and fetch it on the next pass.
			return reconcile.Result{Requeue: true}, nil
		}
		return reconcile.Result{}, err
	}
	// Service exists, check if it's updated.
	// Note: We leave Spec.ClusterIP unspecified for the master to set.
	//       Copy it from foundService to satisfy reflect.DeepEqual.
	service.Spec.ClusterIP = foundService.Spec.ClusterIP
	if !reflect.DeepEqual(foundService.Spec, service.Spec) {
		// Specs aren't equal, update and fix.
		reqLogger.Info("Updating Service", "foundService.Spec", foundService.Spec, "service.Spec", service.Spec)
		foundService.Spec = *service.Spec.DeepCopy()
		if err = r.client.Update(context.TODO(), foundService); err != nil {
			return reconcile.Result{}, err
		}
	}

	// Install Metrics ServiceMonitor
	foundServiceMonitor := &monitoringv1.ServiceMonitor{}
	serviceMonitor := metrics.GenerateServiceMonitor(foundService)
	serviceMonitorName := types.NamespacedName{
		Namespace: serviceMonitor.ObjectMeta.Namespace,
		Name:      serviceMonitor.ObjectMeta.Name,
	}
	if err = r.client.Get(context.TODO(), serviceMonitorName, foundServiceMonitor); err != nil {
		if errors.IsNotFound(err) {
			// Didn't find ServiceMonitor
			reqLogger.Info("Creating ServiceMonitor")
			// Note, GenerateServiceMonitor already set an owner reference.
			if err = r.client.Create(context.TODO(), serviceMonitor); err != nil {
				return reconcile.Result{}, err
			}
		} else {
			return reconcile.Result{}, err
		}
	} else {
		// ServiceMonitor exists, check if it's updated.
		if !reflect.DeepEqual(foundServiceMonitor.Spec, serviceMonitor.Spec) {
			// Specs aren't equal, update and fix.
			reqLogger.Info("Updating ServiceMonitor", "foundServiceMonitor.Spec", foundServiceMonitor.Spec, "serviceMonitor.Spec", serviceMonitor.Spec)
			foundServiceMonitor.Spec = *serviceMonitor.Spec.DeepCopy()
			if err = r.client.Update(context.TODO(), foundServiceMonitor); err != nil {
				return reconcile.Result{}, err
			}
		}
	}

	return reconcile.Result{}, nil
}

func credentialsRequest(namespace, name, partitionID, bucketName string) *minterv1.CredentialsRequest {
	codec, _ := minterv1.NewCodec()
	awsProvSpec, _ := codec.EncodeProviderSpec(
		&minterv1.AWSProviderSpec{
			TypeMeta: metav1.TypeMeta{
				Kind: "AWSProviderSpec",
			},
			StatementEntries: []minterv1.StatementEntry{
				{
					Effect: "Allow",
					Action: []string{
						"ec2:DescribeVolumes",
						"ec2:DescribeSnapshots",
						"ec2:CreateTags",
						"ec2:CreateVolume",
						"ec2:CreateSnapshot",
						"ec2:DeleteSnapshot",
					},
					Resource: "*",
				},
				{
					Effect: "Allow",
					Action: []string{
						"s3:GetObject",
						"s3:DeleteObject",
						"s3:PutObject",
						"s3:AbortMultipartUpload",
						"s3:ListMultipartUploadParts",
					},
					Resource: fmt.Sprintf("arn:%s:s3:::%s/*", partitionID, bucketName),
				},
				{
					Effect: "Allow",
					Action: []string{
						"s3:ListBucket",
					},
					Resource: fmt.Sprintf("arn:%s:s3:::%s", partitionID, bucketName),
				},
			},
		})

	return &minterv1.CredentialsRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		TypeMeta: metav1.TypeMeta{
			Kind:       "CredentialsRequest",
			APIVersion: minterv1.SchemeGroupVersion.String(),
		},
		Spec: minterv1.CredentialsRequestSpec{
			SecretRef: corev1.ObjectReference{
				Name:      name,
				Namespace: namespace,
			},
			ProviderSpec: awsProvSpec,
		},
	}
}

func veleroDeployment(namespace string, veleroImageRegistry string) *appsv1.Deployment {
	deployment := veleroInstall.Deployment(namespace,
		veleroInstall.WithEnvFromSecretKey(strings.ToUpper(awsCredsSecretIDKey), credentialsRequestName, awsCredsSecretIDKey),
		veleroInstall.WithEnvFromSecretKey(strings.ToUpper(awsCredsSecretAccessKey), credentialsRequestName, awsCredsSecretAccessKey),
		veleroInstall.WithPlugins([]string{veleroImageRegistry + "/" + veleroAwsImageTag}),
		veleroInstall.WithImage(veleroImageRegistry + "/" + veleroImageTag),
	)

	replicas := int32(1)
	terminationGracePeriodSeconds := int64(30)
	revisionHistoryLimit := int32(2)
	progressDeadlineSeconds := int32(600)
	maxUnavailable := intstr.FromString("25%")
	maxSurge := intstr.FromString("25%")
	deployment.Spec.Replicas = &replicas
	deployment.Spec.RevisionHistoryLimit = &revisionHistoryLimit
	deployment.Spec.ProgressDeadlineSeconds = &progressDeadlineSeconds
	deployment.Spec.Template.Spec.InitContainers[0].TerminationMessagePath = "/dev/termination-log"
	deployment.Spec.Template.Spec.InitContainers[0].TerminationMessagePolicy = "File"
	deployment.Spec.Template.Spec.Containers[0].Env[1].ValueFrom.FieldRef.APIVersion = "v1"
	deployment.Spec.Template.Spec.Containers[0].Ports[0].Protocol = "TCP"
	deployment.Spec.Template.Spec.Containers[0].TerminationMessagePath = "/dev/termination-log"
	deployment.Spec.Template.Spec.Containers[0].TerminationMessagePolicy = "File"
	deployment.Spec.Template.Spec.DeprecatedServiceAccount = "velero"
	deployment.Spec.Template.Spec.DNSPolicy = "ClusterFirst"
	deployment.Spec.Template.Spec.SchedulerName = "default-scheduler"
	deployment.Spec.Template.Spec.SecurityContext = &corev1.PodSecurityContext{}
	deployment.Spec.Template.Spec.TerminationGracePeriodSeconds = &terminationGracePeriodSeconds
	deployment.Spec.Strategy = appsv1.DeploymentStrategy{
		Type: appsv1.RollingUpdateDeploymentStrategyType,
		RollingUpdate: &appsv1.RollingUpdateDeployment{
			MaxUnavailable: &maxUnavailable,
			MaxSurge:       &maxSurge,
		},
	}
	deployment.Spec.Template.Spec.Tolerations = []corev1.Toleration{
		{
			Key:      "node-role.kubernetes.io/infra",
			Operator: corev1.TolerationOpExists,
			Effect:   corev1.TaintEffectNoSchedule,
		},
	}
	deployment.Spec.Template.Spec.Affinity = &corev1.Affinity{
		NodeAffinity: &corev1.NodeAffinity{
			PreferredDuringSchedulingIgnoredDuringExecution: []corev1.PreferredSchedulingTerm{
				{
					Weight: 1,
					Preference: corev1.NodeSelectorTerm{
						MatchExpressions: []corev1.NodeSelectorRequirement{
							{
								Key:      "node-role.kubernetes.io/infra",
								Operator: corev1.NodeSelectorOpExists,
							},
						},
					},
				},
			},
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{
					{
						MatchExpressions: []corev1.NodeSelectorRequirement{
							{
								Key:      "beta.kubernetes.io/arch",
								Operator: corev1.NodeSelectorOpIn,
								Values:   []string{"amd64"},
							},
						},
					},
				},
			},
		},
	}

	return deployment
}

func metricsServiceFromDeployment(deployment *appsv1.Deployment) *corev1.Service {
	// Build a list of ServicePorts from the container ports of the
	// deployment's pod template having "metrics" in the port name.
	var servicePorts []corev1.ServicePort
	for _, container := range deployment.Spec.Template.Spec.Containers {
		for _, port := range container.Ports {
			if strings.Contains(port.Name, "metrics") {
				servicePorts = append(servicePorts, corev1.ServicePort{
					Name:       port.Name,
					Protocol:   port.Protocol,
					Port:       port.ContainerPort,
					TargetPort: intstr.FromInt(int(port.ContainerPort)),
				})
			}
		}
	}

	// Copy labels from the deployment's pod template.
	serviceSelector := make(map[string]string)
	// XXX Looping in lieu of an ObjectMeta.CopyLabels() method.
	for k, v := range deployment.Spec.Template.ObjectMeta.Labels {
		serviceSelector[k] = v
	}

	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Service",
			APIVersion: corev1.SchemeGroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      deployment.ObjectMeta.Name + "-metrics",
			Namespace: deployment.ObjectMeta.Namespace,
			Labels:    map[string]string{"name": deployment.ObjectMeta.Name},
		},
		Spec: corev1.ServiceSpec{
			Ports:    servicePorts,
			Selector: serviceSelector,
			Type:     corev1.ServiceTypeClusterIP,
		},
	}
}

func determineVeleroImageRegistry(region string) string {
	cnRegion := []string{"cn-north-1", "cn-northwest-1"}

	// Use the image in Chinese mirror if running on AWS China
	for _, v := range cnRegion {
		if region == v {
			return veleroImageRegistryCN
		}
	}

	// Use global image by default
	return veleroImageRegistry
}
