package objstore

import (
	"testing"
	"time"
)

func TestFilterSort(t *testing.T) {
	t0 := time.Now()
	objs := []Object{{"xabcx", 1, t0}, {"zzz", 1, t0.Add(time.Hour)}, {"ABC2", 1, t0.Add(2 * time.Hour)}}
	got, trunc := FilterSort(objs, "ABC", 10)
	if len(got) != 2 || trunc || got[0].Key != "ABC2" || got[1].Key != "xabcx" {
		t.Fatalf("got %+v %v", got, trunc)
	}
	got, trunc = FilterSort(objs, "ABC", 1)
	if len(got) != 1 || !trunc || got[0].Key != "ABC2" {
		t.Fatalf("got %+v %v", got, trunc)
	}
}
