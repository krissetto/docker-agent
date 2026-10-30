package facadetest

import (
	"cmp"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"
)

// Assertions are written against the standard library so this non-test package
// does not depend on the test framework. assert* records a failure and continues;
// require* stops the scenario.

func message(msgAndArgs ...any) string {
	if len(msgAndArgs) == 0 {
		return ""
	}
	format, ok := msgAndArgs[0].(string)
	if !ok {
		return fmt.Sprint(msgAndArgs...)
	}
	return fmt.Sprintf(format, msgAndArgs[1:]...)
}

func assertEqual(tb testing.TB, want, got any, msgAndArgs ...any) {
	tb.Helper()
	if !reflect.DeepEqual(want, got) {
		tb.Errorf("not equal: want %#v, got %#v %s", want, got, message(msgAndArgs...))
	}
}

func assertNotEqual(tb testing.TB, unwanted, got any, msgAndArgs ...any) {
	tb.Helper()
	if reflect.DeepEqual(unwanted, got) {
		tb.Errorf("unexpectedly equal to %#v %s", got, message(msgAndArgs...))
	}
}

func assertTrue(tb testing.TB, cond bool, msgAndArgs ...any) {
	tb.Helper()
	if !cond {
		tb.Errorf("expected true %s", message(msgAndArgs...))
	}
}

func assertFalse(tb testing.TB, cond bool, msgAndArgs ...any) {
	tb.Helper()
	if cond {
		tb.Errorf("expected false %s", message(msgAndArgs...))
	}
}

func assertErrorIs(tb testing.TB, err, target error, msgAndArgs ...any) {
	tb.Helper()
	if !errors.Is(err, target) {
		tb.Errorf("error %v does not match %v %s", err, target, message(msgAndArgs...))
	}
}

func assertGreaterOrEqual[T cmp.Ordered](tb testing.TB, got, least T, msgAndArgs ...any) {
	tb.Helper()
	if got < least {
		tb.Errorf("%v is less than %v %s", got, least, message(msgAndArgs...))
	}
}

func assertOneOf[T comparable](tb testing.TB, got T, allowed ...T) {
	tb.Helper()
	if slices.Contains(allowed, got) {
		return
	}
	tb.Errorf("%v is not one of %v", got, allowed)
}

func requireNoError(tb testing.TB, err error) {
	tb.Helper()
	if err != nil {
		tb.Fatalf("unexpected error: %v", err)
	}
}

func requireTrue(tb testing.TB, cond bool, msgAndArgs ...any) {
	tb.Helper()
	if !cond {
		tb.Fatalf("expected true %s", message(msgAndArgs...))
	}
}

func requireNotEmpty[T any](tb testing.TB, value []T, msgAndArgs ...any) {
	tb.Helper()
	if len(value) == 0 {
		tb.Fatalf("expected a non-empty value %s", message(msgAndArgs...))
	}
}

func requireNotEmptyString(tb testing.TB, value string) {
	tb.Helper()
	if value == "" {
		tb.Fatal("expected a non-empty string")
	}
}

func requireNotNil(tb testing.TB, value any, msgAndArgs ...any) {
	tb.Helper()
	if value == nil || reflect.ValueOf(value).IsNil() {
		tb.Fatalf("expected non-nil %s", message(msgAndArgs...))
	}
}

func requireNotZero[T comparable](tb testing.TB, value T, msgAndArgs ...any) {
	tb.Helper()
	var zero T
	if value == zero {
		tb.Fatalf("expected a non-zero value %s", message(msgAndArgs...))
	}
}

func requireLen[T any](tb testing.TB, value []T, want int, msgAndArgs ...any) {
	tb.Helper()
	if len(value) != want {
		tb.Fatalf("length %d, want %d %s", len(value), want, message(msgAndArgs...))
	}
}
