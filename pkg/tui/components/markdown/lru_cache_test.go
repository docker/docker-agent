package markdown

import "testing"

func TestLRUCache_BasicGetPut(t *testing.T) {
	c := newLRUCache[string, int](3)

	c.put("a", 1)
	c.put("b", 2)
	c.put("c", 3)

	v, ok := c.get("a")
	if !ok || v != 1 {
		t.Fatalf("expected (1, true), got (%d, %v)", v, ok)
	}
	v, ok = c.get("b")
	if !ok || v != 2 {
		t.Fatalf("expected (2, true), got (%d, %v)", v, ok)
	}
	v, ok = c.get("c")
	if !ok || v != 3 {
		t.Fatalf("expected (3, true), got (%d, %v)", v, ok)
	}
}

func TestLRUCache_Miss(t *testing.T) {
	c := newLRUCache[string, int](2)

	_, ok := c.get("missing")
	if ok {
		t.Fatal("expected miss for non-existent key")
	}
}

func TestLRUCache_Eviction(t *testing.T) {
	c := newLRUCache[string, int](2)

	c.put("a", 1)
	c.put("b", 2)
	// Cache is full: [b, a] (b is most recent)

	c.put("c", 3)
	// "a" should be evicted as least recently used: [c, b]

	_, ok := c.get("a")
	if ok {
		t.Fatal("expected 'a' to be evicted")
	}

	v, ok := c.get("b")
	if !ok || v != 2 {
		t.Fatalf("expected (2, true), got (%d, %v)", v, ok)
	}
	v, ok = c.get("c")
	if !ok || v != 3 {
		t.Fatalf("expected (3, true), got (%d, %v)", v, ok)
	}
}

func TestLRUCache_GetPromotesEntry(t *testing.T) {
	c := newLRUCache[string, int](2)

	c.put("a", 1)
	c.put("b", 2)
	// [b, a]

	// Access "a" to promote it
	c.get("a")
	// Now [a, b]

	// Add "c" - should evict "b" (now least recently used)
	c.put("c", 3)

	_, ok := c.get("b")
	if ok {
		t.Fatal("expected 'b' to be evicted after 'a' was promoted")
	}

	v, ok := c.get("a")
	if !ok || v != 1 {
		t.Fatalf("expected (1, true), got (%d, %v)", v, ok)
	}
}

func TestLRUCache_UpdateExistingKey(t *testing.T) {
	c := newLRUCache[string, int](2)

	c.put("a", 1)
	c.put("b", 2)

	// Update "a"
	c.put("a", 10)

	v, ok := c.get("a")
	if !ok || v != 10 {
		t.Fatalf("expected (10, true), got (%d, %v)", v, ok)
	}

	// "a" was promoted by the update, so adding "c" should evict "b"
	c.put("c", 3)
	_, ok = c.get("b")
	if ok {
		t.Fatal("expected 'b' to be evicted")
	}
}

func TestLRUCache_Clear(t *testing.T) {
	c := newLRUCache[string, int](3)

	c.put("a", 1)
	c.put("b", 2)

	c.clear()

	_, ok := c.get("a")
	if ok {
		t.Fatal("expected empty cache after clear")
	}
	_, ok = c.get("b")
	if ok {
		t.Fatal("expected empty cache after clear")
	}

	// Should work normally after clear
	c.put("c", 3)
	v, ok := c.get("c")
	if !ok || v != 3 {
		t.Fatalf("expected (3, true), got (%d, %v)", v, ok)
	}
}
