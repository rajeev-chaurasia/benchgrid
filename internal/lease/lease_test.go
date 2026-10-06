package lease

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rajeev-chaurasia/benchgrid/internal/store"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("BENCHGRID_TEST_DATABASE_URL")
	if url == "" {
		url = "postgres:///benchgrid_test?sslmode=disable"
	}
	ctx := context.Background()
	pool, err := store.Open(ctx, url, 80)
	if err != nil {
		t.Skipf("no test database: %v", err)
	}
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := store.Reset(ctx, pool); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func addRig(t *testing.T, pool *pgxpool.Pool, id string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `INSERT INTO rigs (id, descriptor) VALUES ($1, '{}')`, id); err != nil {
		t.Fatal(err)
	}
}

func TestFenceIncreasesAcrossGrants(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	addRig(t, pool, "r1")

	a, ok, err := Acquire(ctx, pool, "r1", "a", "e1", 1, time.Minute)
	if err != nil || !ok {
		t.Fatalf("first acquire: %v %v", ok, err)
	}
	if _, ok, _ := Acquire(ctx, pool, "r1", "b", "e2", 1, time.Minute); ok {
		t.Fatal("acquired a held rig")
	}
	if ok, _ := Release(ctx, pool, "r1", a.Fence); !ok {
		t.Fatal("holder could not release")
	}
	b, ok, _ := Acquire(ctx, pool, "r1", "b", "e2", 1, time.Minute)
	if !ok || b.Fence != a.Fence+1 {
		t.Fatalf("fence %d after %d", b.Fence, a.Fence)
	}
}

func TestExpiredLeaseIsTakenAndOldFenceIsPowerless(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	addRig(t, pool, "r1")

	old, _, _ := Acquire(ctx, pool, "r1", "a", "e1", 1, 50*time.Millisecond)
	time.Sleep(120 * time.Millisecond)
	cur, ok, _ := Acquire(ctx, pool, "r1", "b", "e2", 1, time.Minute)
	if !ok {
		t.Fatal("expired lease was not taken over")
	}
	if ok, _ := Renew(ctx, pool, "r1", old.Fence, time.Minute); ok {
		t.Error("stale holder renewed")
	}
	if ok, _ := Release(ctx, pool, "r1", old.Fence); ok {
		t.Error("stale holder released the new holder's lease")
	}
	if ok, _ := Renew(ctx, pool, "r1", cur.Fence, time.Minute); !ok {
		t.Error("current holder could not renew")
	}
}

func TestLapsedButUntakenLeaseRenews(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	addRig(t, pool, "r1")

	g, _, _ := Acquire(ctx, pool, "r1", "a", "e1", 1, 50*time.Millisecond)
	time.Sleep(120 * time.Millisecond)
	if ok, _ := Renew(ctx, pool, "r1", g.Fence, time.Minute); !ok {
		t.Error("a lapsed lease nobody took must still renew for its fence")
	}
}

func TestQuarantinedRigIsNotGranted(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	addRig(t, pool, "r1")
	pool.Exec(ctx, `UPDATE rigs SET agent_state = 'QUARANTINED'`)
	if _, ok, _ := Acquire(ctx, pool, "r1", "a", "e1", 1, time.Minute); ok {
		t.Error("granted a quarantined rig")
	}
}

// The full race with interval accounting lives in the evidence harness. This
// is the quick version that runs with every test: many callers, one rig, one
// winner per release.
func TestConcurrentAcquireHasOneWinner(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	addRig(t, pool, "r1")

	const callers = 64
	var wg sync.WaitGroup
	var mu sync.Mutex
	winners := 0
	gate := make(chan struct{})
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-gate
			_, ok, err := Acquire(ctx, pool, "r1", "x", "e", 1, time.Minute)
			if err != nil {
				t.Error(err)
			}
			if ok {
				mu.Lock()
				winners++
				mu.Unlock()
			}
		}()
	}
	close(gate)
	wg.Wait()
	if winners != 1 {
		t.Fatalf("%d winners", winners)
	}
}
