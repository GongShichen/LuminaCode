package agent

import "strconv"

const maxCompactChars = 50_000

func TruncateResult(content string, maxChars ...int) string {
	limit := maxCompactChars
	if len(maxChars) > 0 {
		limit = maxChars[0]
	}
	runes := []rune(content)
	if len(runes) <= limit {
		return content
	}
	half := floorDiv(limit, 2)
	removed := len(runes) - limit
	return string(runes[:pythonSliceEnd(len(runes), half)]) + "\n\n... [" + strconv.Itoa(removed) + " characters truncated] ...\n\n" + string(runes[pythonSliceStart(len(runes), -half):])
}

func floorDiv(value, divisor int) int {
	result := value / divisor
	if value%divisor != 0 && ((value < 0) != (divisor < 0)) {
		result--
	}
	return result
}

func pythonSliceEnd(length, end int) int {
	if end < 0 {
		end = length + end
	}
	if end < 0 {
		return 0
	}
	if end > length {
		return length
	}
	return end
}

func pythonSliceStart(length, start int) int {
	if start < 0 {
		start = length + start
	}
	if start < 0 {
		return 0
	}
	if start > length {
		return length
	}
	return start
}
