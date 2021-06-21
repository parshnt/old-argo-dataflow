// +build test

package e2e

import (
	. "github.com/argoproj-labs/argo-dataflow/api/v1alpha1"
	. "github.com/argoproj-labs/argo-dataflow/test"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"testing"
)

func TestSTAN(t *testing.T) {

	Setup(t)
	defer Teardown(t)

	subject := RandomSTANSubject()

	CreatePipeline(Pipeline{
		ObjectMeta: metav1.ObjectMeta{Name: "stan"},
		Spec: PipelineSpec{
			Steps: []StepSpec{
				{
					Name:    "a",
					Cat:     &Cat{},
					Sources: []Source{{HTTP: &HTTPSource{}}},
					Sinks:   []Sink{{STAN: &STAN{Subject: subject}}},
				},
				{
					Name:    "b",
					Cat:     &Cat{},
					Sources: []Source{{STAN: &STAN{Subject: subject}}},
					Sinks:   []Sink{{Log: &Log{}}},
				},
			},
		},
	})

	cancel := StartPortForward("stan-a-0")
	defer cancel()

	SendMessageViaHTTP("my-msg")

	WaitForPipeline(UntilMessagesSunk)

	ExpectLogLine("stan-b-0", "sidecar", "my-msg")
}