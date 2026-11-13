package main

import (
	"errors"
	"fmt"
	"testing"
)

func TestIngressFailureStageCannotReflectPrivateErrorContent(t *testing.T) {
	for _, err := range []error{nil, errors.New("PRIVATE_PAYLOAD"), ingressFailure("PRIVATE_PAYLOAD"), ingressFailure("flow_read\nPRIVATE_PAYLOAD")} {
		if ingressFailureStage(err) != "observation" {
			t.Fatal("unrecognized error content escaped the closed stage vocabulary")
		}
	}
	if ingressFailureStage(fmt.Errorf("PRIVATE_PAYLOAD: %w", ingressFailure("flow_read"))) != "flow_read" {
		t.Fatal("a wrapped closed failure stage was lost")
	}
}
