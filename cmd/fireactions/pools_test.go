package main

import "testing"

func TestPoolsScaleRejectsNegativeAndOverflowBeforeConnecting(t *testing.T) {
	cmd := newPoolsScaleCmd()
	if err := cmd.Flags().Set("replicas", "-1"); err != nil {
		t.Fatal(err)
	}
	if err := runPoolsScaleCmd(cmd, []string{"pool"}); err == nil {
		t.Fatal("expected negative replicas to be rejected")
	}

	cmd = newPoolsScaleCmd()
	if err := cmd.Flags().Set("replicas", "2147483648"); err != nil {
		t.Fatal(err)
	}
	if err := runPoolsScaleCmd(cmd, []string{"pool"}); err == nil {
		t.Fatal("expected int32 overflow to be rejected")
	}
}
