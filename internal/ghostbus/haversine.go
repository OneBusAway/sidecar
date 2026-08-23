package ghostbus

import "math"

// earthRadiusM matches OBACloud's Haversine helper so exported distances
// agree between the two implementations.
const earthRadiusM = 6371000.0

// HaversineMeters is the great-circle distance between two WGS84 points.
// Used only for the CSV's vehicle_distance_from_stop_m column; callers must
// not call it with defaulted zero coordinates (0,0 is a real place).
func HaversineMeters(lat1, lon1, lat2, lon2 float64) float64 {
	toRad := func(d float64) float64 { return d * math.Pi / 180 }
	dLat := toRad(lat2 - lat1)
	dLon := toRad(lon2 - lon1)
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(toRad(lat1))*math.Cos(toRad(lat2))*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * earthRadiusM * math.Asin(math.Sqrt(a))
}
