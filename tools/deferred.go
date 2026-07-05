package tools

import (
	"sort"
	"strings"
)

type DeferredToolIndex struct {
	tools map[string]Tool
	order []string
}

func NewDeferredToolIndex() *DeferredToolIndex {
	return &DeferredToolIndex{tools: map[string]Tool{}}
}

func (d *DeferredToolIndex) Add(tool Tool) {
	if tool == nil {
		return
	}
	if _, exists := d.tools[tool.Name()]; !exists {
		d.order = append(d.order, tool.Name())
	}
	d.tools[tool.Name()] = tool
}

func (d *DeferredToolIndex) GetAll() map[string]Tool {
	out := make(map[string]Tool, len(d.tools))
	for k, v := range d.tools {
		out[k] = v
	}
	return out
}

func (d *DeferredToolIndex) Names() []string {
	names := make([]string, 0, len(d.order))
	for _, name := range d.order {
		if _, ok := d.tools[name]; ok {
			names = append(names, name)
		}
	}
	return names
}

func (d *DeferredToolIndex) Activate(name string) Tool {
	if tool, ok := d.tools[name]; ok {
		delete(d.tools, name)
		d.removeOrder(name)
		return tool
	}
	return nil
}

func (d *DeferredToolIndex) Search(query string) []Tool {
	if query == "" || len(d.tools) == 0 {
		return nil
	}
	if strings.HasPrefix(query, "select:") {
		var out []Tool
		for _, name := range strings.Split(query[7:], ",") {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			if tool, ok := d.tools[name]; ok {
				out = append(out, tool)
			}
		}
		return out
	}
	prefix := ""
	var keywords []string
	if strings.HasPrefix(query, "+") {
		parts := strings.SplitN(query[1:], " ", 2)
		prefix = strings.ToLower(parts[0])
		if len(parts) > 1 {
			keywords = strings.Fields(strings.ToLower(parts[1]))
		}
	} else {
		keywords = strings.Fields(strings.ToLower(query))
	}
	type scored struct {
		tool  Tool
		score int
	}
	var matches []scored
	for _, name := range d.order {
		tool, ok := d.tools[name]
		if !ok {
			continue
		}
		if prefix != "" && !strings.HasPrefix(strings.ToLower(name), prefix) {
			continue
		}
		score := deferredToolScore(tool, keywords)
		if score > 0 || len(keywords) == 0 {
			matches = append(matches, scored{tool: tool, score: score})
		}
	}
	sort.SliceStable(matches, func(i, j int) bool {
		return matches[i].score > matches[j].score
	})
	out := make([]Tool, 0, len(matches))
	for _, item := range matches {
		out = append(out, item.tool)
	}
	return out
}

func (d *DeferredToolIndex) removeOrder(name string) {
	for i, item := range d.order {
		if item == name {
			d.order = append(d.order[:i], d.order[i+1:]...)
			return
		}
	}
}

func deferredToolScore(tool Tool, keywords []string) int {
	if len(keywords) == 0 {
		return 1
	}
	nameLower := strings.ToLower(tool.Name())
	hintLower := strings.ToLower(tool.SearchHint())
	score := 0
	for _, keyword := range keywords {
		if strings.Contains(nameLower, keyword) {
			score += 3
		}
		if strings.Contains(hintLower, keyword) {
			score++
		}
	}
	return score
}
