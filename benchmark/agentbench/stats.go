package agentbench

import (
	"math"
	"sort"
)

func Average(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	var total float64
	for _, value := range values {
		total += value
	}
	return total / float64(len(values))
}

func Percentile(values []float64, percentile float64) *float64 {
	if len(values) == 0 {
		return nil
	}
	if percentile <= 0 {
		percentile = 0
	}
	if percentile >= 100 {
		percentile = 100
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	if len(sorted) == 1 {
		return floatPtr(sorted[0])
	}
	rank := (percentile / 100) * float64(len(sorted)-1)
	low := int(math.Floor(rank))
	high := int(math.Ceil(rank))
	if low == high {
		return floatPtr(sorted[low])
	}
	fraction := rank - float64(low)
	value := sorted[low] + (sorted[high]-sorted[low])*fraction
	return floatPtr(value)
}

func latencySummary(values []float64) LatencySummary {
	return LatencySummary{
		P50: Percentile(values, 50),
		P90: Percentile(values, 90),
		P95: Percentile(values, 95),
	}
}

func floatPtr(value float64) *float64 {
	return &value
}
