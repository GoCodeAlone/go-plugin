// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

// The plugin package exposes functions and helpers for communicating to
// plugins which are implemented as standalone binary applications.
//
// plugin.Client fully manages the lifecycle of executing the application,
// connecting to it, and returning the RPC client for dispensing plugins.
//
// plugin.Serve fully manages listeners to expose an RPC server from a binary
// that plugin.Client can connect to.
package plugin

import (
	"context"
	"google.golang.org/grpc"
)

// Plugin is the interface that is implemented to serve/connect to
// a plugin over gRPC.
type Plugin interface {
	// GRPCServer should register this plugin for serving with the
	// given GRPCServer. Unlike Plugin.Server, this is only called once
	// since gRPC plugins serve singletons.
	GRPCServer(*GRPCBroker, *grpc.Server) error

	// GRPCClient should return the interface implementation for the plugin
	// you're serving via gRPC. The provided context will be canceled by
	// go-plugin in the event of the plugin process exiting.
	GRPCClient(context.Context, *GRPCBroker, *grpc.ClientConn) (any, error)
}
