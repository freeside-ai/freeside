package domain

import (
	"strings"
	"testing"
)

// enumValidity is the internal-package half of the enum tests: it needs the
// unexported valid() predicate. Every registered member is valid, and the zero
// value (empty string) is always invalid.
func TestEnumValidity(t *testing.T) {
	valids := map[string][]func() bool{}
	invalids := map[string]func() bool{}

	for _, v := range AllAttentionTypes {
		valids["AttentionType"] = append(valids["AttentionType"], v.valid)
	}
	invalids["AttentionType"] = AttentionType("").valid
	for _, v := range AllSubjectTypes {
		valids["SubjectType"] = append(valids["SubjectType"], v.valid)
	}
	invalids["SubjectType"] = SubjectType("").valid
	for _, v := range AllProducerClasses {
		valids["ProducerClass"] = append(valids["ProducerClass"], v.valid)
	}
	invalids["ProducerClass"] = ProducerClass("").valid
	for _, v := range AllDeliveryStatuses {
		valids["DeliveryStatus"] = append(valids["DeliveryStatus"], v.valid)
	}
	invalids["DeliveryStatus"] = DeliveryStatus("").valid
	for _, v := range AllInterruptionClasses {
		valids["InterruptionClass"] = append(valids["InterruptionClass"], v.valid)
	}
	invalids["InterruptionClass"] = InterruptionClass("").valid
	for _, v := range AllActions {
		valids["Action"] = append(valids["Action"], v.valid)
	}
	invalids["Action"] = Action("").valid
	for _, v := range AllPriorities {
		valids["Priority"] = append(valids["Priority"], v.valid)
	}
	invalids["Priority"] = Priority("").valid
	for _, v := range AllItemStatuses {
		valids["ItemStatus"] = append(valids["ItemStatus"], v.valid)
	}
	invalids["ItemStatus"] = ItemStatus("").valid
	for _, v := range AllSensitivityClasses {
		valids["SensitivityClass"] = append(valids["SensitivityClass"], v.valid)
	}
	invalids["SensitivityClass"] = SensitivityClass("").valid
	for _, v := range AllHeadBindings {
		valids["HeadBinding"] = append(valids["HeadBinding"], v.valid)
	}
	invalids["HeadBinding"] = HeadBinding("").valid
	for _, v := range AllAuthors {
		valids["Author"] = append(valids["Author"], v.valid)
	}
	invalids["Author"] = Author("").valid
	for _, v := range AllProvenanceSources {
		valids["ProvenanceSource"] = append(valids["ProvenanceSource"], v.valid)
	}
	invalids["ProvenanceSource"] = ProvenanceSource("").valid

	for name, checks := range valids {
		for i, check := range checks {
			if !check() {
				t.Errorf("%s member %d: valid() = false, want true", name, i)
			}
		}
	}
	for name, check := range invalids {
		if check() {
			t.Errorf("%s zero value: valid() = true, want false", name)
		}
	}
}

// TestDeliveryStatusVocabulary is acceptance criterion 3: the delivery-status
// vocabulary never calls a channel provider's acceptance "delivered". No member
// is or contains that word, and the vocabulary is exactly the three honest
// statuses.
func TestDeliveryStatusVocabulary(t *testing.T) {
	want := map[DeliveryStatus]bool{
		DeliverySubmitted:       true,
		DeliveryChannelAccepted: true,
		DeliveryOpened:          true,
	}
	if len(AllDeliveryStatuses) != len(want) {
		t.Fatalf("AllDeliveryStatuses = %v, want exactly %d honest statuses", AllDeliveryStatuses, len(want))
	}
	for _, s := range AllDeliveryStatuses {
		if !want[s] {
			t.Errorf("unexpected delivery status %q", s)
		}
		if strings.Contains(strings.ToLower(string(s)), "deliver") {
			t.Errorf("delivery status %q implies delivery; channel acceptance is never called delivered", s)
		}
	}
	// No status maps acceptance to "delivered": the accepted status is a
	// distinct, weaker word.
	if DeliveryChannelAccepted == "delivered" {
		t.Error("channel acceptance must not be represented as delivered")
	}
}
