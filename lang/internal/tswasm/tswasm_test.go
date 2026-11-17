package tswasm_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ronny/brisk/lang/internal/tswasm"
)

func TestParseHonorsDeadline(t *testing.T) {
	var b strings.Builder
	b.WriteString("package big\n\n")
	for i := 0; b.Len() < 512<<10; i++ {
		fmt.Fprintf(&b, "func F%d(a, b int) int {\n\tif a > b && a != %d {\n\t\treturn a*b + %d\n\t}\n\treturn b\n}\n", i, i, i)
	}
	src := []byte(b.String())
	if _, err := tswasm.Parse(context.Background(), tswasm.Go, []byte("package warm\n")); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := tswasm.Parse(context.Background(), tswasm.Go, src); err != nil {
		t.Fatal(err)
	}
	full := time.Since(start)

	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	start = time.Now()
	_, err := tswasm.Parse(ctx, tswasm.Go, src)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Parse with a 1 ms deadline: err = %v, want DeadlineExceeded", err)
	}
	if d := time.Since(start); d > full/2 {
		t.Errorf("Parse with a 1 ms deadline took %v; a full parse takes %v", d, full)
	}

	// A waiter gives up too, instead of blocking until the lock frees.
	done, cancel2 := context.WithCancel(context.Background())
	cancel2()
	if _, err := tswasm.Parse(done, tswasm.Go, src); !errors.Is(err, context.Canceled) {
		t.Fatalf("Parse with a cancelled context: err = %v, want Canceled", err)
	}

	// The next parse gets a working module.
	tree, err := tswasm.Parse(context.Background(), tswasm.Go, []byte("package x\n\nfunc F() {}\n"))
	if err != nil {
		t.Fatalf("Parse after a cancelled parse: %v", err)
	}
	if len(tree.Nodes) < 5 || tree.Nodes[0].HasError() {
		t.Fatalf("Parse after a cancelled parse: %d nodes, error %v", len(tree.Nodes), tree.Nodes[0].HasError())
	}
}
