// Command smoke drives one sandbox through the whole SDK surface against a
// live Bonya API and edge, printing one PASS/FAIL/INFO line per check. It is
// the SDK's kept demo (run by scripts/smoke.sh); it deletes what it creates.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	bonya "github.com/bonyai/bonya-sdk-go"
)

var failures int

func pass(format string, args ...any) { fmt.Printf("PASS  "+format+"\n", args...) }
func info(format string, args ...any) { fmt.Printf("INFO  "+format+"\n", args...) }
func fail(format string, args ...any) {
	failures++
	fmt.Printf("FAIL  "+format+"\n", args...)
}

func check(name string, err error) bool {
	if err != nil {
		fail("%s: %v", name, err)
		return false
	}
	pass("%s", name)
	return true
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	client, err := bonya.NewClient()
	if err != nil {
		fmt.Println("FAIL  client:", err)
		os.Exit(1)
	}
	if image := os.Getenv("BONYA_SMOKE_IMAGE"); image != "" {
		imageSmoke(ctx, client, image)
		fmt.Printf("RESULT %d failure(s)\n", failures)
		if failures > 0 {
			os.Exit(1)
		}
		return
	}

	// The key owner's organizations (API keys may list them since the beta fixes).
	if orgs, err := client.ListOrganizations(ctx); err != nil {
		info("list organizations with an API key: %v (expected before the beta fixes rollout)", err)
	} else {
		pass("list organizations with an API key (%d)", len(orgs))
	}

	// Errors carry kinds, never statuses.
	if _, err := client.Get(ctx, "sb-00000000000000"); errors.Is(err, bonya.ErrNotFound) {
		pass("get unknown sandbox is ErrNotFound")
	} else {
		fail("get unknown sandbox: want ErrNotFound, got %v", err)
	}

	begun := time.Now()
	sb, err := client.Create(ctx, bonya.CreateOptions{Name: "sdk-smoke"})
	if !check("create", err) {
		os.Exit(1)
	}
	info("created %s in %s, state %s", sb.ID, time.Since(begun).Round(time.Millisecond), sb.State)
	if sb.Image == nil {
		pass("default create has no image metadata")
	} else {
		fail("default create unexpectedly has image metadata: %+v", sb.Image)
	}
	var cleanup []*bonya.Sandbox
	cleanup = append(cleanup, sb)
	defer func() {
		for _, s := range cleanup {
			dctx, dcancel := context.WithTimeout(context.Background(), 2*time.Minute)
			if err := s.Delete(dctx); err != nil && !errors.Is(err, bonya.ErrNotFound) {
				fail("delete %s: %v", s.ID, err)
			} else {
				pass("delete %s", s.ID)
			}
			dcancel()
		}
		fmt.Printf("RESULT %d failure(s)\n", failures)
		if failures > 0 {
			os.Exit(1)
		}
	}()

	if page, err := client.List(ctx, bonya.ListOptions{States: []bonya.State{bonya.StateRunning}}); err == nil {
		found := false
		for _, s := range page.Sandboxes {
			found = found || s.ID == sb.ID
		}
		if found {
			pass("list running includes the new sandbox")
		} else {
			fail("list running: %s not on the first page", sb.ID)
		}
	} else {
		fail("list: %v", err)
	}

	if c, err := sb.Capability(ctx); err != nil {
		info("capability not issued: %v (exec and files use the API key)", err)
	} else {
		pass("capability issued, expires %s", c.ExpiresAt.Format(time.RFC3339))
	}

	// Exec.
	if r, err := sb.Exec(ctx, "sh", "-c", "echo hello; echo oops >&2"); check("exec", err) {
		if strings.TrimSpace(r.Stdout) == "hello" && strings.TrimSpace(r.Stderr) == "oops" && r.ExitCode == 0 {
			pass("exec output and exit code (%s)", r.Duration.Round(time.Millisecond))
		} else {
			fail("exec result: stdout %q stderr %q exit %d", r.Stdout, r.Stderr, r.ExitCode)
		}
	}
	if r, err := sb.Exec(ctx, "sh", "-c", "exit 3"); err != nil {
		fail("exec exit 3: %v", err)
	} else {
		var exitErr *bonya.ExitError
		if errors.As(r.Check(), &exitErr) && r.ExitCode == 3 {
			pass("non-zero exit reported by Check")
		} else {
			fail("exit 3: code %d, Check %v", r.ExitCode, r.Check())
		}
	}
	var out bytes.Buffer
	if r, err := sb.ExecWithOptions(ctx, bonya.ExecOptions{
		Command: []string{"sh", "-c", "tr a-z A-Z"},
		Stdin:   strings.NewReader("streamed stdin\n"),
		Stdout:  &out,
	}); check("streaming exec with stdin", err) {
		if strings.TrimSpace(out.String()) == "STREAMED STDIN" && r.ExitCode == 0 {
			pass("streaming exec output")
		} else {
			fail("streaming exec output %q exit %d", out.String(), r.ExitCode)
		}
	}
	timeoutBegun := time.Now()
	_, err = sb.ExecWithOptions(ctx, bonya.ExecOptions{
		Command: []string{"sh", "-c", "echo $$ > /tmp/smoke-timeout.pid; while true; do date >> /tmp/smoke-timeout.log; sleep 0.1; done"},
		Timeout: 2 * time.Second,
	})
	took := time.Since(timeoutBegun).Round(100 * time.Millisecond)
	switch {
	case errors.Is(err, bonya.ErrTimeout):
		pass("exec past its timeout is ErrTimeout after %s", took)
		time.Sleep(time.Second)
		r1, _ := sb.Exec(ctx, "sh", "-c", "wc -l < /tmp/smoke-timeout.log")
		time.Sleep(time.Second)
		r2, _ := sb.Exec(ctx, "sh", "-c", "wc -l < /tmp/smoke-timeout.log; kill -0 $(cat /tmp/smoke-timeout.pid) 2>/dev/null && echo alive || echo gone")
		if r1 != nil && r2 != nil {
			lines2 := strings.Fields(r2.Stdout)
			if len(lines2) == 2 && lines2[0] == strings.TrimSpace(r1.Stdout) && lines2[1] == "gone" {
				pass("timed-out command was stopped (log stopped at %s lines)", lines2[0])
			} else {
				fail("timed-out command still running: before %q after %q", r1.Stdout, r2.Stdout)
			}
		}
	case err != nil:
		info("exec past its timeout: %v after %s (server without the exec-timeout fix)", err, took)
		_, _ = sb.Exec(ctx, "sh", "-c", "kill $(cat /tmp/smoke-timeout.pid) 2>/dev/null; true")
	default:
		fail("exec past its timeout returned no error")
	}

	// Files.
	payload := []byte("bonya sdk smoke\n")
	if _, err := sb.WriteFile(ctx, "/tmp/smoke/a.txt", payload); err != nil {
		// The parent may need to exist first.
		_ = sb.Mkdir(ctx, "/tmp/smoke")
		_, err = sb.WriteFile(ctx, "/tmp/smoke/a.txt", payload)
		check("write file (after mkdir)", err)
	} else {
		pass("write file")
	}
	if data, err := sb.ReadFile(ctx, "/tmp/smoke/a.txt", 0); check("read file", err) && !bytes.Equal(data, payload) {
		fail("read file content %q", data)
	}
	if st, err := sb.Stat(ctx, "/tmp/smoke/a.txt"); check("stat", err) && st.Size != int64(len(payload)) {
		fail("stat size %d", st.Size)
	}
	check("rename", sb.Rename(ctx, "/tmp/smoke/a.txt", "/tmp/smoke/b.txt"))
	if entries, err := sb.ListDir(ctx, "/tmp/smoke"); check("list dir", err) {
		if len(entries) != 1 || entries[0].Name != "b.txt" {
			fail("list dir entries %+v", entries)
		}
	}
	tmp, _ := os.MkdirTemp("", "bonya-smoke")
	defer os.RemoveAll(tmp)
	big := bytes.Repeat([]byte("0123456789abcdef"), 1<<16) // 1 MiB
	local := filepath.Join(tmp, "up.bin")
	_ = os.WriteFile(local, big, 0o600)
	if _, err := sb.Upload(ctx, local, "/tmp/smoke/up.bin"); check("upload 1 MiB", err) {
		down := filepath.Join(tmp, "down.bin")
		if _, err := sb.Download(ctx, "/tmp/smoke/up.bin", down); check("download 1 MiB", err) {
			got, _ := os.ReadFile(down)
			if !bytes.Equal(got, big) {
				fail("download content differs (%d bytes)", len(got))
			} else {
				pass("upload/download bytes identical")
			}
		}
	}
	check("remove all", sb.RemoveAll(ctx, "/tmp/smoke"))
	if _, err := sb.Stat(ctx, "/tmp/smoke"); errors.Is(err, bonya.ErrNotFound) {
		pass("stat removed path is ErrNotFound")
	} else {
		fail("stat removed path: want ErrNotFound, got %v", err)
	}

	// Terminal session.
	if _, err := sb.CreateSession(ctx, bonya.SessionOptions{Name: "smoke", Command: []string{"sh", "-c", "echo session-ready; sleep 120"}}); check("create session", err) {
		if list, err := sb.ListSessions(ctx); check("list sessions", err) && len(list) == 0 {
			fail("list sessions empty")
		}
		var term syncBuffer
		att, err := sb.AttachSession(ctx, "smoke", bonya.AttachOptions{Output: &term, NoReconnect: true})
		if check("attach session", err) {
			deadline := time.Now().Add(5 * time.Second)
			for !strings.Contains(term.String(), "session-ready") && time.Now().Before(deadline) {
				time.Sleep(100 * time.Millisecond)
			}
			if strings.Contains(term.String(), "session-ready") {
				pass("attach replayed session output")
			} else {
				fail("attach output %q", term.String())
			}
			check("detach", att.Detach())
		}
		_, err = sb.KillSession(ctx, "smoke", bonya.KillSessionOptions{})
		check("kill session", err)
	}

	// Preview and grants.
	if p, err := sb.CreatePreview(ctx, 8000, bonya.PreviewOptions{}); check("create private preview", err) {
		info("preview url %s", p.URL)
		if p.Grant == nil || p.Grant.Token == "" {
			fail("private preview without a grant token")
		}
		if g, err := p.CreateGrant(ctx, 10*time.Minute); check("create grant", err) {
			check("renew grant", g.Renew(ctx, 20*time.Minute))
			check("revoke grant", g.Revoke(ctx))
		}
		check("delete preview", p.Delete(ctx))
	}

	// Pause and wake: the handle's token keeps working.
	if check("pause warm", sb.Pause(ctx, bonya.PauseWarm)) {
		if _, err := sb.Keepalive(ctx, 10*time.Minute); check("keepalive (wake)", err) {
			check("wait running", sb.WaitRunning(ctx))
			if r, err := sb.Exec(ctx, "echo", "after-wake"); check("exec after wake", err) && strings.TrimSpace(r.Stdout) != "after-wake" {
				fail("exec after wake output %q", r.Stdout)
			}
		}
	}

	// Snapshot and restore.
	_, _ = sb.WriteFile(ctx, "/root/marker.txt", []byte("from-snapshot"))
	if snap, err := sb.CreateSnapshot(ctx, bonya.SnapshotOptions{Name: "sdk-smoke"}); check("create snapshot (published)", err) {
		restored, err := client.Create(ctx, bonya.CreateOptions{Name: "sdk-smoke-restore", Snapshot: snap.ID})
		if check("restore from snapshot", err) {
			cleanup = append(cleanup, restored)
			if data, err := restored.ReadFile(ctx, "/root/marker.txt", 0); check("read restored file", err) && string(data) != "from-snapshot" {
				fail("restored file %q", data)
			}
			check("delete restored", restored.Delete(ctx))
			cleanup = cleanup[:len(cleanup)-1]
		}
		if err := snap.Delete(ctx); err != nil {
			info("delete snapshot: %v", err)
		} else {
			pass("delete snapshot")
		}
	}

	// Fork.
	if res, err := sb.Fork(ctx, bonya.ForkOptions{Count: 1}); check("fork", err) && len(res.Sandboxes) == 1 {
		child := res.Sandboxes[0]
		cleanup = append(cleanup, child)
		if r, err := child.Exec(ctx, "cat", "/root/marker.txt"); check("exec in fork child", err) && strings.TrimSpace(r.Stdout) != "from-snapshot" {
			fail("fork child file %q", r.Stdout)
		}
	}
}

// syncBuffer is a bytes.Buffer safe for the attach goroutine and the poller.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
