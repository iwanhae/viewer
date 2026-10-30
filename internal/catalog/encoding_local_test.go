package catalog

import (
	"context"
	"encoding/hex"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

const localEncodingToken = "0123456789abcdef0123456789abcdef"

func dropEncodingOwnershipColumn(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.db.Exec(`ALTER TABLE blobs DROP COLUMN encoding_owner`); err != nil {
		t.Fatal(err)
	}
}

func TestLocalEncodingExactClaimAndFencing(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedClaimBlobs(t, s, "target", "other")
	until := time.Now().Add(time.Hour)
	job, status, err := s.ClaimLocalEncoding(ctx, "target", localEncodingToken, until)
	if err != nil || status != LocalEncodingClaimed || job.Hash != "target" || job.Token != localEncodingToken || job.Owner != "local" || job.StageKey != "" {
		t.Fatalf("claim=%+v %s %v", job, status, err)
	}
	other, _ := s.GetEncodingJob(ctx, "other")
	if other.Status != "pending" || other.Token != "" {
		t.Fatalf("other hash mutated: %+v", other)
	}
	if ok, err := s.SkipEncoding(ctx, "target", "wrong", "skipped", job.Type, "", job.SizeBytes); err != nil || ok {
		t.Fatalf("wrong token skip=%v %v", ok, err)
	}
	if err := s.ReleaseLocalEncoding(ctx, "target", "wrong"); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseEncoding(ctx, "target", job.Token); err != nil {
		t.Fatal(err)
	}
	current, _ := s.GetEncodingJob(ctx, "target")
	if current.Token != job.Token || current.Status != "leased" {
		t.Fatalf("wrong owner released local: %+v", current)
	}
	if err := s.ReleaseLocalEncoding(ctx, "target", job.Token); err != nil {
		t.Fatal(err)
	}
	current, _ = s.GetEncodingJob(ctx, "target")
	if current.Status != "pending" || current.Owner != "external" || current.Token != "" {
		t.Fatalf("release=%+v", current)
	}
}

func TestLocalEncodingBusyMissingAndInvalidTokens(t *testing.T) {
	for _, state := range []string{"leased", "received", "committing", "done", "failed", "skipped", "webp", "missing"} {
		t.Run(state, func(t *testing.T) {
			s := openTestStore(t)
			ctx := context.Background()
			if state != "missing" {
				seedClaimBlobs(t, s, "target")
				if state == "webp" {
					_, _ = s.db.Exec(`UPDATE blobs SET content_type='image/webp' WHERE hash='target'`)
				} else {
					// Even an expired external lease may not be stolen locally.
					_, _ = s.db.Exec(`UPDATE blobs SET encoding_status=?,encoding_token=?,encoding_lease_until=1 WHERE hash='target'`, state, localEncodingToken)
				}
			}
			before, _ := s.GetEncodingJob(ctx, "target")
			_, status, err := s.ClaimLocalEncoding(ctx, "target", localEncodingToken, time.Now().Add(time.Hour))
			want := LocalEncodingBusy
			if state == "missing" {
				want = LocalEncodingMissing
			}
			if err != nil || status != want {
				t.Fatalf("status=%s err=%v", status, err)
			}
			if err := s.ReleaseLocalEncoding(ctx, "target", localEncodingToken); err != nil {
				t.Fatal(err)
			}
			after, _ := s.GetEncodingJob(ctx, "target")
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("busy/missing changed: before=%+v after=%+v", before, after)
			}
		})
	}
	s := openTestStore(t)
	for _, token := range []string{"", "../secret", "run-item-rand-text", "zz23456789abcdef0123456789abcdef"} {
		if _, _, err := s.ClaimLocalEncoding(context.Background(), "target", token, time.Now().Add(time.Hour)); err == nil {
			t.Fatalf("unsafe token accepted: %q", token)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := s.ClaimLocalEncoding(ctx, "target", localEncodingToken, time.Now().Add(time.Hour)); err == nil {
		t.Fatal("cancelled claim succeeded")
	}
}

func TestLocalEncodingAcceptedProtectedFromRunAndOperatorReset(t *testing.T) {
	for _, state := range []string{"received", "committing", "leased"} {
		t.Run(state, func(t *testing.T) {
			s := openTestStore(t)
			ctx := context.Background()
			seedClaimBlobs(t, s, "target")
			r := createTestRun(t, s, "encoding", "key")
			if err := s.StartJobRun(ctx, r.ID); err != nil {
				t.Fatal(err)
			}
			item := nextTestRunItem(t, s, r.ID)
			if raw, err := hex.DecodeString(item.ClaimToken); err != nil || len(raw) != 16 {
				t.Fatalf("item token not safe 128-bit hex: %q", item.ClaimToken)
			}
			if _, _, err := s.ClaimLocalEncoding(ctx, item.Hash, item.ClaimToken, time.Now().Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(`UPDATE blobs SET encoding_status=?,encoding_lease_until=1 WHERE hash='target'`, state); err != nil {
				t.Fatal(err)
			}
			if _, err := s.ResetStuckEncodings(ctx, nil); err != nil {
				t.Fatal(err)
			}
			claimed, err := s.ClaimEncoding(ctx, 1, time.Minute)
			if err != nil || len(claimed) != 0 {
				t.Fatalf("external stole local %s: %+v %v", state, claimed, err)
			}
			if err := s.FinishJobRun(ctx, r.ID, "interrupted", "service_stopped"); err != nil {
				t.Fatal(err)
			}
			job, _ := s.GetEncodingJob(ctx, item.Hash)
			want := state
			if state == "leased" {
				want = "pending"
			}
			if job.Status != want {
				t.Fatalf("run cleanup state=%s want=%s", job.Status, want)
			}
		})
	}
}

func TestEncodingOwnershipMigrationPreservesExternalClaims(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.db")
	s, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	seedClaimBlobs(t, s, "external")
	jobs, err := s.ClaimEncoding(context.Background(), 1, time.Hour)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("external claim: %+v %v", jobs, err)
	}
	dropEncodingOwnershipColumn(t, s)
	if _, err := s.db.Exec(`PRAGMA user_version=11`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	job, err := s.GetEncodingJob(context.Background(), "external")
	if err != nil || job.Token != jobs[0].Token || job.StageKey != jobs[0].StageKey || job.Status != "leased" || job.Owner != "external" || job.LeaseUntil.UnixMilli() != jobs[0].LeaseUntil.UnixMilli() {
		t.Fatalf("migration changed external claim: %+v %v", job, err)
	}
}

func TestEncodingRunCleanupRequiresLocalOwnerEvenWithMatchingToken(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedClaimBlobs(t, s, "target")
	r := createTestRun(t, s, "encoding", "key")
	if err := s.StartJobRun(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	item := nextTestRunItem(t, s, r.ID)
	if _, err := s.db.Exec(`UPDATE blobs SET encoding_status='leased',encoding_owner='external',encoding_token=?,encoding_lease_until=? WHERE hash=?`, item.ClaimToken, time.Now().Add(time.Hour).UnixMilli(), item.Hash); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishJobRun(ctx, r.ID, "interrupted", "service_stopped"); err != nil {
		t.Fatal(err)
	}
	job, _ := s.GetEncodingJob(ctx, item.Hash)
	if job.Status != "leased" || job.Owner != "external" || job.Token != item.ClaimToken {
		t.Fatalf("run cleanup released external owner: %+v", job)
	}
}
