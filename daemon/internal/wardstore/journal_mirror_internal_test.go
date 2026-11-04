package wardstore

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// The handoff journal is copied field by field between ward's record and the
// store's, and journal records are ward's recovery authority: a field one
// copy misses reads back as zero. These tests fill every field of each side
// and require the round trip to return it unchanged, so a field added to
// either record, or to one of its nested structs, fails here until the
// conversion copies it. MarkStatePrepared and MarkInstructionsPrepared build
// their store values through the same helpers toStoreRecord uses, so the
// round trip covers their conversions too.

func TestHandoffJournalRecordRoundTripsFromWard(t *testing.T) {
	assertMirrorRoundTrip(t, func(f *mirrorFiller) bool {
		var rec ward.HandoffJournalRecord
		f.fill(reflect.ValueOf(&rec).Elem(), "HandoffJournalRecord")
		return assertSameRecord(t, f.bit, rec, fromStoreRecord(toStoreRecord(rec)))
	})
}

func TestHandoffJournalRecordRoundTripsFromStore(t *testing.T) {
	assertMirrorRoundTrip(t, func(f *mirrorFiller) bool {
		var rec store.HandoffJournalRecord
		f.fill(reflect.ValueOf(&rec).Elem(), "HandoffJournalRecord")
		return assertSameRecord(t, f.bit, rec, toStoreRecord(fromStoreRecord(rec)))
	})
}

// assertMirrorRoundTrip runs one round trip per bit of the bool ordinals,
// until every bool has been true in some pass and every pair of bools has
// differed in some pass. One pass with every bool true couldn't tell a
// copy that swaps two bools from a correct one.
func assertMirrorRoundTrip(t *testing.T, roundTrip func(*mirrorFiller) bool) {
	t.Helper()
	for bit := uint(0); ; bit++ {
		f := &mirrorFiller{t: t, bit: bit}
		if !roundTrip(f) {
			return
		}
		if f.bools>>(bit+1) == 0 {
			return
		}
	}
}

func assertSameRecord[T any](t *testing.T, bit uint, want, got T) bool {
	t.Helper()
	if reflect.DeepEqual(got, want) {
		return true
	}
	var diffs []string
	diffMirrorValues(reflect.ValueOf(want), reflect.ValueOf(got), "HandoffJournalRecord", &diffs)
	t.Errorf("bool pass %d: round trip lost or moved fields:", bit)
	for _, diff := range diffs {
		t.Errorf("  %s", diff)
	}
	return false
}

// mirrorFiller sets every field it reaches to a non-zero value. Strings,
// integers, and times get values distinct from every other field's, so a
// copy that reads the wrong source field fails. The bool at ordinal n
// (counted from 1) is set to bit `bit` of n. A field kind it doesn't know
// fails the test, so a new slice, map, or unexported field can't slip
// through as zero.
type mirrorFiller struct {
	t     *testing.T
	bit   uint
	next  int
	bools uint
}

var timeType = reflect.TypeFor[time.Time]()

func (f *mirrorFiller) fill(v reflect.Value, path string) {
	f.t.Helper()
	if !v.CanSet() {
		f.t.Fatalf("%s: cannot set an unexported field; extend the mirror test", path)
	}
	f.next++
	switch {
	case v.Type() == timeType:
		v.Set(reflect.ValueOf(time.Date(2026, 1, 1, 0, 0, f.next, 0, time.UTC)))
		return
	case v.Kind() == reflect.String:
		v.SetString(fmt.Sprintf("%s-%d", path, f.next))
		return
	case v.CanInt():
		v.SetInt(int64(f.next))
		return
	case v.Kind() == reflect.Bool:
		f.bools++
		v.SetBool(f.bools>>f.bit&1 == 1)
		return
	case v.Kind() == reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		f.fill(v.Elem(), path)
		return
	case v.Kind() == reflect.Struct:
		for i := range v.NumField() {
			f.fill(v.Field(i), path+"."+v.Type().Field(i).Name)
		}
		return
	}
	f.t.Fatalf("%s: mirror test cannot fill kind %s; extend mirrorFiller.fill", path, v.Kind())
}

// diffMirrorValues names each leaf path where want and got differ, walking
// the same kinds mirrorFiller fills.
func diffMirrorValues(want, got reflect.Value, path string, diffs *[]string) {
	switch {
	case want.Type() != timeType && want.Kind() == reflect.Struct:
		for i := range want.NumField() {
			diffMirrorValues(want.Field(i), got.Field(i), path+"."+want.Type().Field(i).Name, diffs)
		}
	case want.Kind() == reflect.Pointer && !want.IsNil() && !got.IsNil():
		diffMirrorValues(want.Elem(), got.Elem(), path, diffs)
	case !reflect.DeepEqual(want.Interface(), got.Interface()):
		*diffs = append(*diffs, fmt.Sprintf("%s: want %v, got %v", path, describeMirrorValue(want), describeMirrorValue(got)))
	}
}

func describeMirrorValue(v reflect.Value) string {
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return "nil"
		}
		return "&" + describeMirrorValue(v.Elem())
	}
	return fmt.Sprintf("%v", v.Interface())
}
