package store

import (
	"context"
	"os"
	"testing"
)

// 需要 FOOTNOTE_TEST_DATABASE_URL（見 internal/gap 的整合測試）；沒設就略過。
func TestTryLockIsExclusive(t *testing.T) {
	url := os.Getenv("FOOTNOTE_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("FOOTNOTE_TEST_DATABASE_URL 未設定，略過整合測試")
	}
	ctx := context.Background()
	st, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	release, ok, err := st.TryLock(ctx, 42)
	if err != nil || !ok {
		t.Fatalf("first lock: ok=%v err=%v", ok, err)
	}
	if _, ok, err := st.TryLock(ctx, 42); err != nil || ok {
		t.Fatalf("second lock should fail while held: ok=%v err=%v", ok, err)
	}
	release()
	release2, ok, err := st.TryLock(ctx, 42)
	if err != nil || !ok {
		t.Fatalf("lock after release: ok=%v err=%v", ok, err)
	}
	release2()
}
