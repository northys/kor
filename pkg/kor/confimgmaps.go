package kor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/plugin/pkg/client/auth/oidc"
)

var exceptionconfigmaps = []ExceptionResource{
	{ResourceName: "aws-auth", Namespace: "kube-system"},
	{ResourceName: "kube-root-ca.crt", Namespace: "*"},
}

func retrieveUsedCM(clientset kubernetes.Interface, namespace string) ([]string, []string, []string, []string, []string, []string, error) {
	var volumesCM []string
	var volumesProjectedCM []string
	var envCM []string
	var envFromCM []string
	var envFromContainerCM []string
	var envFromInitContainerCM []string

	pods, err := clientset.CoreV1().Pods(namespace).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return nil, nil, nil, nil, nil, nil, err
	}

	for _, pod := range pods.Items {
		for _, volume := range pod.Spec.Volumes {
			if volume.ConfigMap != nil {
				volumesCM = append(volumesCM, volume.ConfigMap.Name)
			}
			if volume.Projected != nil {
				for _, source := range volume.Projected.Sources {
					if source.ConfigMap != nil {
						volumesProjectedCM = append(volumesProjectedCM, source.ConfigMap.Name)
					}
				}
			}
		}
		for _, container := range pod.Spec.Containers {
			for _, env := range container.Env {
				if env.ValueFrom != nil && env.ValueFrom.ConfigMapKeyRef != nil {
					envCM = append(envCM, env.ValueFrom.ConfigMapKeyRef.Name)
				}
			}
			for _, envFrom := range container.EnvFrom {
				if envFrom.ConfigMapRef != nil {
					envFromCM = append(envFromCM, envFrom.ConfigMapRef.Name)
				}
			}
			for _, envFrom := range container.EnvFrom {
				if envFrom.ConfigMapRef != nil {
					envFromContainerCM = append(envFromContainerCM, envFrom.ConfigMapRef.Name)
				}
			}
		}
		for _, initContainer := range pod.Spec.InitContainers {
			for _, volume := range initContainer.VolumeMounts {
				if volume.Name != "" && volume.MountPath != "" {
					volumesCM = append(volumesCM, volume.Name)
				}
			}
			for _, env := range initContainer.Env {
				if env.ValueFrom != nil && env.ValueFrom.ConfigMapKeyRef != nil {
					envFromInitContainerCM = append(envFromInitContainerCM, env.ValueFrom.ConfigMapKeyRef.Name)
				}
			}
		}
	}

	for _, resource := range exceptionconfigmaps {
		if resource.Namespace == namespace || resource.Namespace == "*" {
			volumesCM = append(volumesCM, resource.ResourceName)
		}
	}

	return volumesCM, volumesProjectedCM, envCM, envFromCM, envFromContainerCM, envFromInitContainerCM, nil
}

func retrieveConfigMapNames(clientset kubernetes.Interface, namespace string, filterOpts *FilterOptions) ([]string, error) {
	configmaps, err := clientset.CoreV1().ConfigMaps(namespace).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(configmaps.Items))
	for _, configmap := range configmaps.Items {
		// checks if the resource has any labels that match the excluded selector specified in opts.ExcludeLabels.
		// If it does, the resource is skipped.
		if excluded, _ := HasExcludedLabel(configmap.Labels, filterOpts.ExcludeLabels); excluded {
			continue
		}
		// checks if the resource's age (measured from its last modified time) matches the included criteria
		// specified by the filter options.
		if included, _ := HasIncludedAge(configmap.CreationTimestamp, filterOpts); !included {
			continue
		}

		if configmap.Labels["kor/used"] == "true" {
			continue
		}

		names = append(names, configmap.Name)
	}
	return names, nil
}

func processNamespaceCM(clientset kubernetes.Interface, namespace string, filterOpts *FilterOptions) ([]string, error) {
	volumesCM, volumesProjectedCM, envCM, envFromCM, envFromContainerCM, envFromInitContainerCM, err := retrieveUsedCM(clientset, namespace)
	if err != nil {
		return nil, err
	}

	volumesCM = RemoveDuplicatesAndSort(volumesCM)
	volumesProjectedCM = RemoveDuplicatesAndSort(volumesProjectedCM)
	envCM = RemoveDuplicatesAndSort(envCM)
	envFromCM = RemoveDuplicatesAndSort(envFromCM)
	envFromContainerCM = RemoveDuplicatesAndSort(envFromContainerCM)
	envFromInitContainerCM = RemoveDuplicatesAndSort(envFromInitContainerCM)

	configMapNames, err := retrieveConfigMapNames(clientset, namespace, filterOpts)
	if err != nil {
		return nil, err
	}

	var usedConfigMaps []string
	slicesToAppend := [][]string{volumesCM, volumesProjectedCM, envCM, envFromCM, envFromContainerCM, envFromInitContainerCM}

	for _, slice := range slicesToAppend {
		usedConfigMaps = append(usedConfigMaps, slice...)
	}
	diff := CalculateResourceDifference(usedConfigMaps, configMapNames)
	return diff, nil

}

func GetUnusedConfigmaps(includeExcludeLists IncludeExcludeLists, filterOpts *FilterOptions, clientset kubernetes.Interface, outputFormat string, opts Opts) (string, error) {
	var outputBuffer bytes.Buffer
	namespaces := SetNamespaceList(includeExcludeLists, clientset)
	response := make(map[string]map[string][]string)

	for _, namespace := range namespaces {
		diff, err := processNamespaceCM(clientset, namespace, filterOpts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to process namespace %s: %v\n", namespace, err)
			continue
		}

		if opts.DeleteFlag {
			if diff, err = DeleteResource(diff, clientset, namespace, "ConfigMap", opts.NoInteractive); err != nil {
				fmt.Fprintf(os.Stderr, "Failed to delete ConfigMap %s in namespace %s: %v\n", diff, namespace, err)
			}
		}
		output := FormatOutput(namespace, diff, "Configmaps")
		outputBuffer.WriteString(output)
		outputBuffer.WriteString("\n")

		resourceMap := make(map[string][]string)
		resourceMap["ConfigMap"] = diff
		response[namespace] = resourceMap
	}

	jsonResponse, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return "", err
	}

	unusedCMs, err := unusedResourceFormatter(outputFormat, outputBuffer, opts, jsonResponse)
	if err != nil {
		fmt.Printf("err: %v\n", err)
	}

	return unusedCMs, nil
}
