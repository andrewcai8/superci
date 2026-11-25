package cache

import (
	"reflect"
	"testing"
	"time"
)

func TestEvictTakesOtherScopesOldestFirstThenTheDefaultBranch(t *testing.T) {
	at := time.Unix(1000, 0)
	objs := []Object{
		{Key: "v1/d/1/main/v/old", Size: 10, Modified: at},
		{Key: "v1/r/1/pr1/v/a", Size: 10, Modified: at.Add(2 * time.Hour)},
		{Key: "v1/r/1/pr2/v/b", Size: 10, Modified: at.Add(1 * time.Hour)},
		{Key: "v1/d/1/main/v/new", Size: 10, Modified: at.Add(3 * time.Hour)},
	}
	if got := Evict(objs, 40); got != nil {
		t.Fatalf("under the cap: %v", got)
	}
	if got, want := Evict(objs, 25), []string{"v1/r/1/pr2/v/b", "v1/r/1/pr1/v/a"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got, want := Evict(objs, 5), []string{"v1/r/1/pr2/v/b", "v1/r/1/pr1/v/a", "v1/d/1/main/v/old", "v1/d/1/main/v/new"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
