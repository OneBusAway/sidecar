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
			// Splitting one rectangle into four quadrants of equal area must
			// not move the centroid. This is the property that rules out the
			// unweighted mean, and the reason weighting is by area.
			name: "split invariance",
			bounds: []directoryBound{
				{Lat: 9.95, Lon: 19.95, LatSpan: 0.1, LonSpan: 0.1},
				{Lat: 9.95, Lon: 20.05, LatSpan: 0.1, LonSpan: 0.1},
				{Lat: 10.05, Lon: 19.95, LatSpan: 0.1, LonSpan: 0.1},
				{Lat: 10.05, Lon: 20.05, LatSpan: 0.1, LonSpan: 0.1},
			},
			want: &LatLon{Lat: 10, Lon: 20},
		},
		{
			// A large rectangle beside a tiny one is dominated by the large
			// one. An unweighted mean would sit halfway between them.
			name: "area weighting dominates",
			bounds: []directoryBound{
				{Lat: 0, Lon: 0, LatSpan: 10, LonSpan: 10},
				{Lat: 50, Lon: 50, LatSpan: 0.01, LonSpan: 0.01},
			},
			want: &LatLon{Lat: 0.00005, Lon: 0.00005},
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

	const epsilon = 1e-4
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
