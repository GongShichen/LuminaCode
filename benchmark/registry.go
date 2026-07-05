package benchmark

import (
	"log"
	"sort"
)

type BenchmarkRegistry struct {
	plugins map[string]BenchmarkPlugin
}

func NewBenchmarkRegistry() *BenchmarkRegistry {
	return &BenchmarkRegistry{plugins: map[string]BenchmarkPlugin{}}
}

func (r *BenchmarkRegistry) Register(name string, plugin BenchmarkPlugin) {
	if r.plugins == nil {
		r.plugins = map[string]BenchmarkPlugin{}
	}
	r.plugins[name] = plugin
}

func (r *BenchmarkRegistry) Get(name string) BenchmarkPlugin {
	if r == nil {
		return missingBenchmarkPlugin(name)
	}
	plugin, ok := r.plugins[name]
	if !ok {
		return missingBenchmarkPlugin(name)
	}
	return plugin
}

func missingBenchmarkPlugin(name string) BenchmarkPlugin {
	log.Panic(name)
	return nil
}

func (r *BenchmarkRegistry) Names() []string {
	if r == nil {
		return nil
	}
	names := make([]string, 0, len(r.plugins))
	for name := range r.plugins {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
