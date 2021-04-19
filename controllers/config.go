package controllers

import (
	"fmt"
	dfv1 "github.com/argoproj-labs/argo-dataflow/api/v1alpha1"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

var (
	logger         = zap.New()
	imageFormat    = os.Getenv("IMAGE_FORMAT")
	runnerImage    = ""
	pullPolicy     = corev1.PullPolicy(os.Getenv("PULL_POLICY"))
	updateInterval = 15 * time.Second
	uninstallAfter = 5 * time.Minute
	installer      = os.Getenv("INSTALLER") == "true"
)

func init() {
	if imageFormat == "" {
		imageFormat = "quay.io/argoproj/%s:latest"
	}
	runnerImage = fmt.Sprintf(imageFormat, "dataflow-runner")
	if v, ok := os.LookupEnv("UNINSTALL_AFTER"); ok {
		if v, err := time.ParseDuration(v); err != nil {
			panic(err)
		} else {
			uninstallAfter = v
		}
	}
	if v, ok := os.LookupEnv(dfv1.EnvUpdateInterval); ok {
		if v, err := time.ParseDuration(v); err != nil {
			panic(err)
		} else {
			updateInterval = v
		}
	}
	logger.Info("config",
		"imageFormat", imageFormat,
		"runnerImage", runnerImage,
		"pullPolicy", pullPolicy,
		"installer", installer,
		"uninstallAfter", uninstallAfter.String(),
		"updateInterval", updateInterval.String())
}
