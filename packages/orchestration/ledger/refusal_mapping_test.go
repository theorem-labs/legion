package ledger

import (
	"errors"
	"strings"
	"testing"
)

func TestRefusalForViolationMapsEveryKnownKind(t *testing.T) {
	for _, kind := range violationKinds {
		t.Run(kind, func(t *testing.T) {
			err := refusalForViolation(Violation{Kind: kind, Message: "candidate refused"})
			var refusal *Refusal
			if !errors.As(err, &refusal) {
				t.Fatalf("%s: want refusal, got %v", kind, err)
			}
			if refusal.Reason != kind {
				t.Fatalf("%s: refusal reason %q", kind, refusal.Reason)
			}
		})
	}
}

func TestRefusalForViolationRejectsUnmappedKind(t *testing.T) {
	err := refusalForViolation(Violation{Kind: "future_kind", Message: "candidate refused"})
	var refusal *Refusal
	if errors.As(err, &refusal) || !strings.Contains(err.Error(), "unmapped violation kind") {
		t.Fatalf("unmapped violation: %v", err)
	}
}
