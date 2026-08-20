package regions

import "testing"

func TestComputeCentroid(t *testing.T) {
	tests := []struct {
		name   string
		bounds []directoryBound
		want   *LatLon
	}{
		{
			name:   "no bounds yields nil",
			bounds: nil,
			want:   nil,
		},
		{
			name:   "single rectangle is its own center",
			bounds: []directoryBound{{Lat: 38.5449, Lon: -121.7444, LatSpan: 0.1, LonSpan: 0.1}},
			want:   &LatLon{Lat: 38.5449, Lon: -121.7444},
		},
		{
			// A 10x10 rectangle at (0,0) beside a 1x1 rectangle at (10,10)
			// has unequal areas (100 vs 1), so the three plausible reductions
			// diverge: area-weighted mean = 10/101 ~= 0.0990, unweighted mean
			// of centers = (5,5), union bbox center = (2.75,2.75). Only the
			// area-weighted answer belongs here. (An equal-area split, like
			// four same-size quadrants, can't distinguish these -- all three
			// algorithms agree on that fixture, so it wouldn't catch a
			// regression to either alternative.)
			name: "split invariance: unsplit",
			bounds: []directoryBound{
				{Lat: 0, Lon: 0, LatSpan: 10, LonSpan: 10},
				{Lat: 10, Lon: 10, LatSpan: 1, LonSpan: 1},
			},
			want: &LatLon{Lat: 10.0 / 101, Lon: 10.0 / 101},
		},
		{
			// Same coverage as "unsplit" above, but the 10x10 rectangle is
			// cut into four 5x5 quadrants that recombine to the same area.
			// The area-weighted mean must land on the same point as the
			// unsplit form (10/101, matching the case above); the unweighted
			// mean would move to (2,2) on this fixture, which is the
			// split-invariance property the comment above describes.
			name: "split invariance: split into quadrants",
			bounds: []directoryBound{
				{Lat: -2.5, Lon: -2.5, LatSpan: 5, LonSpan: 5},
				{Lat: -2.5, Lon: 2.5, LatSpan: 5, LonSpan: 5},
				{Lat: 2.5, Lon: -2.5, LatSpan: 5, LonSpan: 5},
				{Lat: 2.5, Lon: 2.5, LatSpan: 5, LonSpan: 5},
				{Lat: 10, Lon: 10, LatSpan: 1, LonSpan: 1},
			},
			want: &LatLon{Lat: 10.0 / 101, Lon: 10.0 / 101},
		},
		{
			// A large rectangle beside a tiny one is dominated by the large
			// one. An unweighted mean would sit halfway between them.
			// Expected value verified independently: (0*100 + 50*0.0001) /
			// (100 + 0.0001) = 4.999995000005e-05.
			name: "area weighting dominates",
			bounds: []directoryBound{
				{Lat: 0, Lon: 0, LatSpan: 10, LonSpan: 10},
				{Lat: 50, Lon: 50, LatSpan: 0.01, LonSpan: 0.01},
			},
			want: &LatLon{Lat: 4.999995e-05, Lon: 4.999995e-05},
		},
		{
			// Every span zero: fall back to the unweighted mean rather than
			// dividing by zero.
			name: "zero spans fall back to unweighted mean",
			bounds: []directoryBound{
				{Lat: 10, Lon: 20},
				{Lat: 20, Lon: 40},
			},
			want: &LatLon{Lat: 15, Lon: 30},
		},
		{
			name:   "out of range result is nil",
			bounds: []directoryBound{{Lat: 91, Lon: 0, LatSpan: 1, LonSpan: 1}},
			want:   nil,
		},
		{
			name:   "out of range longitude is nil",
			bounds: []directoryBound{{Lat: 0, Lon: 181, LatSpan: 1, LonSpan: 1}},
			want:   nil,
		},
		{
			// Negative spans are nonsense from upstream; clamping them to zero
			// weight keeps them from subtracting area.
			name: "negative spans contribute no weight",
			bounds: []directoryBound{
				{Lat: 10, Lon: 20, LatSpan: 2, LonSpan: 2},
				{Lat: 80, Lon: 170, LatSpan: -5, LonSpan: -5},
			},
			want: &LatLon{Lat: 10, Lon: 20},
		},
	}

	// 1e-9 is loose relative to float64 precision at these magnitudes (~1e-16
	// relative error) but tight enough that it cannot be satisfied by an
	// unrelated constant or a wrong algorithm landing near the right answer
	// by coincidence -- unlike the 1e-4 this replaced, which was larger than
	// the smallest expected value in the table.
	const epsilon = 1e-9
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computeCentroid(tt.bounds)
			if tt.want == nil {
				if got != nil {
					t.Fatalf("computeCentroid = %+v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("computeCentroid = nil, want %+v", tt.want)
			}
			if diff := got.Lat - tt.want.Lat; diff > epsilon || diff < -epsilon {
				t.Errorf("Lat = %v, want %v", got.Lat, tt.want.Lat)
			}
			if diff := got.Lon - tt.want.Lon; diff > epsilon || diff < -epsilon {
				t.Errorf("Lon = %v, want %v", got.Lon, tt.want.Lon)
			}
		})
	}
}
