package growthlimit

import "testing"

func TestBudgetShrinkPreservesAdmittedAndRejectsNew(t *testing.T) {
	b := New(8)
	for range 8 {
		if !b.Enter() {
			t.Fatal("could not admit configured capacity")
		}
	}
	if b.Enter() {
		t.Fatal("exceeded capacity")
	}
	if err := b.Set(2); err != nil {
		t.Fatal(err)
	}
	if b.LeaseLimit() != 8 || b.Current() != 2 || b.Enter() {
		t.Fatal("shrink revoked admission or admitted excess")
	}
	for range 7 {
		b.Leave()
	}
	if !b.Enter() || b.Enter() || b.LeaseLimit() != 2 {
		t.Fatal("new limit not enforced after drain")
	}
	b.Leave()
	b.Leave()
	if err := b.Set(129); err == nil || b.Current() != 2 {
		t.Fatal("invalid change applied")
	}
	if err := b.Set(0); err == nil {
		t.Fatal("zero accepted")
	}
}

func TestBudgetDefaultAndIncrease(t *testing.T) {
	b := New(0)
	if b.Current() != 16 {
		t.Fatal("default must allow more than four")
	}
	if err := b.Set(128); err != nil {
		t.Fatal(err)
	}
	for range 128 {
		if !b.Enter() {
			t.Fatal("hidden admission limit")
		}
	}
	if b.Enter() || b.LeaseLimit() != 128 {
		t.Fatal("maximum mismatch")
	}
	for range 128 {
		b.Leave()
	}
}
