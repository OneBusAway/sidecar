package ghostbus

import "testing"

func TestValidWaitDuration(t *testing.T) {
	for _, v := range []int64{5, 10, 15, 20, 30} {
		if !ValidWaitDuration(v) {
			t.Errorf("ValidWaitDuration(%d) = false, want true", v)
		}
	}
	// 25 is the classic off-by-one a range check would wrongly accept;
	// 0 and negatives guard the "absent coerced to zero" path.
	for _, v := range []int64{0, -5, 1, 25, 31, 300} {
		if ValidWaitDuration(v) {
			t.Errorf("ValidWaitDuration(%d) = true, want false", v)
		}
	}
}
