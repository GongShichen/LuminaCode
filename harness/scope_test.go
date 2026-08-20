package harness

import "testing"

func TestRuntimeScopeResolutionAndLifecycle(t *testing.T) {
	root := NewScope("global", ScopeGlobal, nil)
	rootDispose, err := root.Provide("model", "root", 0)
	if err != nil {
		t.Fatal(err)
	}
	child, err := root.Child("session-1", ScopeSession)
	if err != nil {
		t.Fatal(err)
	}
	if value, ok := child.Resolve("model"); !ok || value != "root" {
		t.Fatalf("child must inherit root provider: %#v %t", value, ok)
	}
	childDispose, err := child.Provide("model", "child", 10)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := child.Provide("model", "duplicate", 10); err == nil {
		t.Fatal("same-scope duplicate priority must fail")
	}
	if value, _ := child.Resolve("model"); value != "child" {
		t.Fatalf("nearest scope must win: %#v", value)
	}
	childDispose()
	if value, _ := child.Resolve("model"); value != "root" {
		t.Fatalf("dispose must restore parent provider: %#v", value)
	}
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := child.Provide("x", "y", 0); err == nil {
		t.Fatal("closed scope must reject providers")
	}
	rootDispose()
}
