package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	bonya "github.com/bonyai/bonya-sdk-go"
)

// imageSmoke exercises public image creation and metadata through the same SDK
// used by the default smoke, with at most one live image sandbox at a time.
func imageSmoke(ctx context.Context, client *bonya.Client, reference string) {
	if _, err := client.Create(ctx, bonya.CreateOptions{Image: reference, Snapshot: "snp-00000000000000"}); errors.Is(err, bonya.ErrInvalidRequest) {
		pass("image and snapshot together are ErrInvalidRequest")
	} else {
		fail("image plus snapshot: want ErrInvalidRequest, got %v", err)
	}
	if _, err := client.Create(ctx, bonya.CreateOptions{Image: "invalid image"}); errors.Is(err, bonya.ErrInvalidRequest) {
		pass("invalid image reference is ErrInvalidRequest")
	} else {
		fail("invalid image reference: want ErrInvalidRequest, got %v", err)
	}
	expired, cancel := context.WithDeadline(ctx, time.Now().Add(-time.Second))
	_, err := client.Create(expired, bonya.CreateOptions{Image: reference})
	cancel()
	if errors.Is(err, context.DeadlineExceeded) {
		pass("image create respects an expired caller deadline")
	} else {
		fail("expired create: want context deadline, got %v", err)
	}

	var sandbox *bonya.Sandbox
	var restored *bonya.Sandbox
	var snapshot *bonya.Snapshot
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if restored != nil {
			check("delete restored image sandbox", restored.Delete(cleanup))
		}
		if sandbox != nil {
			check("delete image sandbox", sandbox.Delete(cleanup))
		}
		if snapshot != nil {
			check("delete image snapshot", snapshot.Delete(cleanup))
		}
	}()

	opts := bonya.CreateOptions{Name: "sdk-image-smoke", Image: reference,
		IdempotencyKey: fmt.Sprintf("sdk-image-smoke-%d", time.Now().UnixNano())}
	begun := time.Now()
	sandbox, err = client.Create(context.Background(), opts)
	if !check("create from image", err) {
		return
	}
	info("created %s from %s in %s", sandbox.ID, reference, time.Since(begun).Round(time.Millisecond))
	if sandbox.Image == nil || sandbox.Image.Reference == "" || !strings.HasPrefix(sandbox.Image.Digest, "sha256:") {
		fail("create image metadata: %+v", sandbox.Image)
		return
	}
	want := *sandbox.Image
	checkImage("create", sandbox.Image, want)
	if body, err := json.Marshal(sandbox); check("marshal image sandbox", err) {
		if strings.Contains(strings.ToLower(string(body)), "runtime") || strings.Contains(string(body), "gvisor") || strings.Contains(string(body), "firecracker") {
			fail("SDK sandbox serialization exposes implementation details")
		} else {
			pass("SDK sandbox serialization exposes no runtime")
		}
	}
	if replay, err := client.Create(ctx, opts); check("repeat image create with same key", err) {
		if replay.ID != sandbox.ID {
			fail("image create replay returned %s, want %s", replay.ID, sandbox.ID)
		} else {
			pass("image create replay returned the same sandbox")
		}
		checkImage("create replay", replay.Image, want)
	}
	if got, err := client.Get(ctx, sandbox.ID); check("get image sandbox", err) {
		checkImage("get", got.Image, want)
	}
	if check("refresh image sandbox", sandbox.Refresh(ctx)) {
		checkImage("refresh", sandbox.Image, want)
	}
	page, err := client.List(ctx, bonya.ListOptions{PageSize: 100})
	if check("list image sandbox", err) {
		found := false
		for _, got := range page.Sandboxes {
			if got.ID == sandbox.ID {
				found = true
				checkImage("list", got.Image, want)
			}
		}
		if !found {
			fail("image sandbox %s missing from first page", sandbox.ID)
		}
	}
	if result, err := sandbox.Exec(ctx, "sh", "-c", "echo image-ready"); check("exec in image sandbox", err) {
		if result.ExitCode == 0 && strings.TrimSpace(result.Stdout) == "image-ready" {
			pass("image exec output and exit")
		} else {
			fail("image exec stdout %q exit %d", result.Stdout, result.ExitCode)
		}
	}
	if _, err := sandbox.Fork(ctx, bonya.ForkOptions{Count: 1}); errors.Is(err, bonya.ErrConflict) &&
		strings.Contains(err.Error(), "image-based") && !strings.Contains(err.Error(), "gVisor") {
		pass("image fork has a typed capability error without runtime details")
	} else {
		fail("image fork: want image-based ErrConflict, got %v", err)
	}
	const marker = "/root/sdk-image-marker.txt"
	if _, err := sandbox.WriteFile(ctx, marker, []byte("sdk-image-snapshot")); !check("write image snapshot marker", err) {
		return
	}
	snapshot, err = sandbox.CreateSnapshot(ctx, bonya.SnapshotOptions{Name: "sdk-image-smoke"})
	if !check("create image snapshot", err) {
		return
	}
	checkImage("snapshot create", snapshot.Image, want)
	if got, err := client.GetSnapshot(ctx, snapshot.ID); check("get image snapshot", err) {
		checkImage("snapshot get", got.Image, want)
	}
	if got, err := sandbox.ListSnapshots(ctx); check("list image snapshots", err) {
		found := false
		for _, s := range got {
			if s.ID == snapshot.ID {
				found = true
				checkImage("snapshot list", s.Image, want)
			}
		}
		if !found {
			fail("snapshot %s missing from source list", snapshot.ID)
		}
	}
	// Release the source's resources before restore: this metadata check does not
	// require two image sandboxes to coexist on the development hosts.
	if !check("delete image source before restore", sandbox.Delete(ctx)) {
		return
	}
	sandbox = nil
	restored, err = client.Create(ctx, bonya.CreateOptions{Name: "sdk-image-restored", Snapshot: snapshot.ID})
	if !check("restore image snapshot without an image argument", err) {
		return
	}
	checkImage("snapshot restore", restored.Image, want)
	if body, err := restored.ReadFile(ctx, marker, 0); check("read restored image marker", err) {
		if string(body) == "sdk-image-snapshot" {
			pass("image snapshot preserved file bytes")
		} else {
			fail("restored image marker %q", body)
		}
	}
	if !check("delete restored before digest create", restored.Delete(ctx)) {
		return
	}
	restored = nil
	base, _, _ := strings.Cut(want.Reference, "@")
	sandbox, err = client.Create(ctx, bonya.CreateOptions{Name: "sdk-image-pinned", Image: base + "@" + want.Digest})
	if !check("create from an exact image digest", err) {
		return
	}
	if sandbox.Image != nil && sandbox.Image.Digest == want.Digest && strings.Contains(sandbox.Image.Reference, "@"+want.Digest) {
		pass("digest create keeps the exact resolved image")
	} else {
		fail("digest create metadata: got %+v, want digest %s", sandbox.Image, want.Digest)
	}
}

func checkImage(operation string, got *bonya.Image, want bonya.Image) {
	if got == nil || *got != want {
		fail("%s image metadata: got %+v, want %+v", operation, got, want)
		return
	}
	pass("%s preserves image reference and digest", operation)
}
