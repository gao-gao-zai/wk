// Package growthrouteerr carries only allowlisted growth route diagnostics.
// It deliberately does not retain or unwrap the original, potentially secret error.
package growthrouteerr

import (
	"context"
	"errors"
)

type Code string

const (
	Unknown                Code = "unknown"
	ManagerClosed          Code = "manager_closed"
	Canceled               Code = "canceled"
	Deadline               Code = "deadline"
	StateUnavailable       Code = "state_unavailable"
	RouteChanged           Code = "route_changed"
	DNSChanged             Code = "dns_changed"
	KernelUnavailable      Code = "kernel_unavailable"
	RouteLimit             Code = "route_limit"
	PortCapacity           Code = "port_capacity"
	KernelInbound          Code = "kernel_inbound"
	KernelRule             Code = "kernel_rule"
	KernelOutbound         Code = "kernel_outbound"
	SelectionFailed        Code = "selection_failed"
	NodeUnavailable        Code = "node_unavailable"
	AcquisitionUnavailable Code = "acquisition_unavailable"
)

var messages = map[Code]string{
	Unknown:                "growth proxy failure",
	ManagerClosed:          "growth proxy manager closed",
	Canceled:               "growth proxy canceled",
	Deadline:               "growth proxy deadline exceeded",
	StateUnavailable:       "growth proxy state unavailable",
	RouteChanged:           "growth proxy route changed after planning",
	DNSChanged:             "growth proxy DNS changed after planning",
	KernelUnavailable:      "growth proxy kernel unavailable",
	RouteLimit:             "growth proxy active route limit reached",
	PortCapacity:           "growth proxy port capacity unavailable (business/probe ports reserved)",
	KernelInbound:          "growth proxy private inbound unavailable",
	KernelRule:             "growth proxy private rule unavailable",
	KernelOutbound:         "growth proxy private outbound unavailable",
	SelectionFailed:        "growth proxy selection failed",
	NodeUnavailable:        "growth proxy node unavailable",
	AcquisitionUnavailable: "growth proxy acquisition unavailable",
}

type routeError struct{ code Code }

func valid(code Code) Code {
	if _, ok := messages[code]; !ok {
		return Unknown
	}
	return code
}

func New(code Code) error           { return &routeError{code: valid(code)} }
func (e *routeError) Error() string { return messages[valid(e.code)] }

// Reason extracts only our concrete error type, never an arbitrary error's text.
func Reason(err error) Code {
	var classified *routeError
	if errors.As(err, &classified) && classified != nil {
		return valid(classified.code)
	}
	if errors.Is(err, context.Canceled) {
		return Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return Deadline
	}
	return Unknown
}

// Public preserves the caller's fixed prefix without retaining the cause.
func Public(prefix string, err error) error {
	return errors.New(prefix + " [reason=" + string(Reason(err)) + "]")
}
