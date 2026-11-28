package smallmodel

import "testing"

// 3 个库各 2 条记录、保持库内次序的交错数 = 6!/(2!2!2!) = 90，
// 且所有交错必须合流到同一最终状态。
func TestExhaustiveConfluence(t *testing.T) {
	out, err := RunExhaustive()
	if err != nil {
		t.Fatalf("合流性被破坏: %v", err)
	}
	if out.Permutations != 90 {
		t.Fatalf("交错穷举数应为 90, got %d", out.Permutations)
	}
	if out.Distinct != 1 || !out.AllConverged {
		t.Fatalf("应只有 1 种最终状态, got %d", out.Distinct)
	}
}

func TestResetFirstConverges(t *testing.T) {
	if err := RunResetFirst(); err != nil {
		t.Fatal(err)
	}
}

func TestCrashAndConflictStableAcrossInterleavings(t *testing.T) {
	if err := RunCrashAndConflict(); err != nil {
		t.Fatal(err)
	}
}
