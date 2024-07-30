/*
Copyright 2024.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"flag"
	"os"

	"github.com/go-logr/logr"
	"go.uber.org/zap/zapcore"
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/reborn1867/k8s-resource-tracer/pkg/webhooks/listener"
)

func main() {
	var debug bool
	flag.BoolVar(&debug, "debug", false, "Enable debug logging")

	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	var logLevel zapcore.Level
	if debug {
		logLevel = zapcore.DebugLevel
	} else {
		logLevel = zapcore.InfoLevel

	}
	logger := zap.New(zap.UseFlagOptions(&opts), zap.Level(logLevel))

	webhookServer := webhook.NewServer(webhook.Options{})

	webhookServer.Register("/listen", &admission.Webhook{Handler: &listener.ListenerWebhook{Logger: logger}, LogConstructor: func(base logr.Logger, req *admission.Request) logr.Logger {
		return logger
	}})

	webhookServer.Register("/healthz", &healthz.CheckHandler{Checker: healthz.Ping})
	webhookServer.Register("/readyz", &healthz.CheckHandler{Checker: healthz.Ping})

	logger.Info("starting k8s resource tracer", "port", 9443)
	if err := webhookServer.Start(context.TODO()); err != nil {
		logger.Error(err, "failed to startk8s resource tracer")
		os.Exit(1)
	}
}
