package Intent

import (
	"context"
	"testing"
	"time"
)

type isolatedMemoryFixture struct{ turns map[string][]StoredTurn }

func (s *isolatedMemoryFixture) Enabled() bool { return true }
func (s *isolatedMemoryFixture) EnsureSession(context.Context, string, string, time.Duration) error {
	return nil
}
func (s *isolatedMemoryFixture) AppendTurn(_ context.Context, session string, turn StoredTurn, _ int, _ time.Duration) error {
	s.turns[session] = append(s.turns[session], turn)
	return nil
}
func (s *isolatedMemoryFixture) LoadRecentTurns(_ context.Context, session string, _ int) ([]StoredTurn, error) {
	return s.turns[session], nil
}

func TestPromptMemoryIsScopedByUserAndDoesNotReadLegacyKeys(t *testing.T) {
	store := &isolatedMemoryFixture{turns: map[string][]StoredTurn{"shared": {{Role: "user", Content: "legacy private content"}}}}
	classifier := &IntentClassifier{memoryStore: store}
	ctx := context.Background()
	classifier.RecordTurn(ctx, "alice", "shared", StoredTurn{Role: "user", Content: "alice private content"})
	alice, err := classifier.loadPromptMemory(ctx, "alice", "shared")
	if err != nil || len(alice.RecentTurns) != 1 || alice.RecentTurns[0].Content != "alice private content" || alice.SessionID != "shared" {
		t.Fatalf("alice memory: %#v %v", alice, err)
	}
	bob, err := classifier.loadPromptMemory(ctx, "bob", "shared")
	if err != nil || len(bob.RecentTurns) != 0 {
		t.Fatalf("cross-user memory leaked: %#v %v", bob, err)
	}
	classifier.RecordTurn(ctx, "bob", "shared", StoredTurn{Role: "user", Content: "bob private content"})
	bob, err = classifier.loadPromptMemory(ctx, "bob", "shared")
	if err != nil || len(bob.RecentTurns) != 1 || bob.RecentTurns[0].Content != "bob private content" {
		t.Fatalf("bob memory: %#v %v", bob, err)
	}
	if len(store.turns["shared"]) != 1 {
		t.Fatal("legacy memory modified")
	}
}

func TestMemoryNamespaceIsCollisionSafe(t *testing.T) {
	pairs := [][2]string{{"a:b", "c"}, {"a", "b:c"}, {"", "shared"}, {"alice", "shared"}, {"bob", "shared"}}
	seen := map[string]bool{}
	for _, pair := range pairs {
		key := scopedMemorySessionID(pair[0], pair[1])
		if seen[key] {
			t.Fatalf("collision for %q", pair)
		}
		seen[key] = true
		if key == pair[1] {
			t.Fatal("legacy key reused")
		}
	}
}
