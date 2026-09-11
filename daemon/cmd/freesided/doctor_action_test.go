package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func TestHTTPOnlyRunDoctorRejectsRetainedArmedSchedule(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	root := t.TempDir()
	h, err := run(ctx, nil, config{
		DBPath:        filepath.Join(root, "freeside.db"),
		FakeDriverDir: filepath.Join(root, "driver"), ListenAddr: "127.0.0.1:0",
	})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := h.fileDurableStop(ctx, errors.New("prior diagnostic scheduler stopped")); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	next := now.Add(time.Hour)
	seconds := int64(3600)
	schedule, err := domain.NewSchedule(domain.ScheduleInput{
		ID: doctorScheduleID, ProjectID: "project-system", Kind: domain.ScheduleDoctor,
		Subject:   domain.ScheduleSubject{Type: domain.ScheduleSubjectTrustedConfig},
		CreatedAt: now, IntervalSeconds: &seconds,
	})
	if err != nil {
		t.Fatal(err)
	}
	var itemID domain.ItemID
	if err := h.store.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutDevice(ctx, domain.Device{
			ID: "doctor-device", DisplayName: "Doctor test", Status: domain.DeviceActive, PairedAt: now,
		}); err != nil {
			return err
		}
		if err := tx.PutSchedule(ctx, schedule); err != nil {
			return err
		}
		if err := tx.SetScheduleTimer(ctx, schedule.ID, schedule.Generation, next); err != nil {
			return err
		}
		items, err := tx.ListOpenAttentionItems(ctx, domain.AttentionSystemHealth)
		for _, item := range items {
			if strings.HasPrefix(string(item.ID), durableStopItemPrefix) {
				itemID = item.ID
			}
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	item, err := h.attention.GetAttentionItem(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.attention.Submit(ctx, signet.ClientCommand{
		CommandID: "doctor-without-consumer", DeviceID: "doctor-device", ExpectedEntityVersion: item.EntityVersion,
		Payload: signet.DecisionPayload{ItemID: itemID, ItemVersion: item.Item.ItemVersion, Action: domain.ActionRunDoctor},
	})
	if err == nil || !strings.Contains(err.Error(), "doctor is unavailable") {
		t.Fatalf("unsupported doctor action = %v", err)
	}
	if err := h.store.Read(ctx, func(tx *store.ReadTx) error {
		if _, err := tx.GetCommand(ctx, "doctor-without-consumer"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("rejected command persisted: %v", err)
		}
		_, got, ok, err := tx.GetScheduleTimer(ctx, schedule.ID)
		if err != nil || !ok || !got.Equal(next) {
			t.Fatalf("rejected request changed timer: %v, %v, %v", got, ok, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
