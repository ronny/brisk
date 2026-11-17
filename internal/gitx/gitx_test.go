package gitx_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ronny/brisk/internal/gitx"
	"github.com/ronny/brisk/risk"
)

const diff = `diff --git a/old/name.go b/new/name.go
similarity index 90%
rename from old/name.go
rename to new/name.go
index 1111111..2222222 100644
--- a/old/name.go
+++ b/new/name.go
@@ -2,4 +2,3 @@ package x
 a
-b
-c
+C
 d
\ No newline at end of file
diff --git a/added.txt b/added.txt
new file mode 100644
--- /dev/null
+++ b/added.txt
@@ -0,0 +1,2 @@
+one
+two
diff --git a/gone.go b/gone.go
deleted file mode 100644
--- a/gone.go
+++ /dev/null
@@ -1 +0,0 @@
-package gone
diff --git a/img.png b/img.png
Binary files a/img.png and b/img.png differ
diff --git "a/sp ace.go" "b/sp ace.go"
--- "a/sp ace.go"
+++ "b/sp ace.go"
@@ -10,0 +11 @@ func f() {
+	x()
`

func TestParseDiff(t *testing.T) {
	files, err := gitx.ParseDiff([]byte(diff))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 5 {
		t.Fatalf("%d files, want 5: %+v", len(files), files)
	}
	r := files[0]
	if r.Path != "new/name.go" || r.OldPath != "old/name.go" || r.Status != risk.Renamed || r.Additions != 1 || r.Deletions != 2 {
		t.Fatalf("rename parsed as %+v", r)
	}
	if got := gitx.AddedLines(r); len(got) != 1 || got[0].N != 3 || got[0].Text != "C" {
		t.Fatalf("added lines %+v, want C at new line 3", got)
	}
	if got := gitx.RemovedLines(r); len(got) != 2 || got[0].N != 3 || got[1].N != 4 {
		t.Fatalf("removed lines %+v, want old lines 3 and 4", got)
	}
	if files[1].Status != risk.Added || files[1].Additions != 2 || files[1].OldPath != "" {
		t.Fatalf("added file %+v", files[1])
	}
	if files[2].Status != risk.Deleted || files[2].Deletions != 1 {
		t.Fatalf("deleted file %+v", files[2])
	}
	if !files[3].Binary || files[3].Path != "img.png" {
		t.Fatalf("binary file %+v", files[3])
	}
	if files[4].Path != "sp ace.go" || files[4].Status != risk.Modified {
		t.Fatalf("quoted path %+v", files[4])
	}
	if got := gitx.AddedLines(files[4]); len(got) != 1 || got[0].N != 11 {
		t.Fatalf("hunk without counts: %+v", got)
	}
}

// Working-tree mode must not read through a symlink to a file outside the
// repository.
func TestChangesDoesNotFollowSymlinks(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v %s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o644)
	run("add", "-A")
	run("commit", "-q", "-m", "base")
	secret := filepath.Join(t.TempDir(), "id_rsa")
	os.WriteFile(secret, []byte("PRIVATE\n"), 0o600)
	if err := os.Symlink(secret, filepath.Join(dir, "key.go")); err != nil {
		t.Fatal(err)
	}
	repo, err := gitx.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := repo.Changes(context.Background(), "HEAD", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range cs.Files {
		if strings.Contains(string(f.New), "PRIVATE") {
			t.Fatalf("%s: loaded the symlink target's contents", f.Path)
		}
	}
	if len(cs.Files) != 1 {
		t.Fatalf("files %+v, want the symlink", cs.Files)
	}
}
