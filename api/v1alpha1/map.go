package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
)

type Map string

func (m Map) GetContainer(runnerImage string, policy corev1.PullPolicy) corev1.Container {
	return corev1.Container{
		Name:            CtrMain,
		Image:           runnerImage,
		ImagePullPolicy: policy,
		Args:            []string{"map", string(m)},
	}
}
