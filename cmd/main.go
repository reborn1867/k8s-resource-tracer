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
	"fmt"
	"os"

	"github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-logr/logr"
	"go.uber.org/zap/zapcore"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/reborn1867/k8s-resource-tracer/pkg/git"
	"github.com/reborn1867/k8s-resource-tracer/pkg/webhooks/listener"
)

func main() {
	var debug bool
	var enableGitReview bool
	var gitURL string
	var gitPath string
	var subPath string
	var branch string

	var logLevel zapcore.Level
	if debug {
		logLevel = zapcore.DebugLevel
	} else {
		logLevel = zapcore.InfoLevel

	}

	opts := zap.Options{
		Development: true,
	}

	logger := zap.New(zap.UseFlagOptions(&opts), zap.Level(logLevel))
	log.SetLogger(logger)

	k8sHost, ok := os.LookupEnv("KUBERNETES_SERVICE_HOST")
	if !ok {
		logger.Error(fmt.Errorf("internal error"), "failed to get env KUBERNETES_SERVICE_HOST")
		os.Exit(1)
	}

	flag.BoolVar(&debug, "debug", false, "Enable debug logging")
	flag.BoolVar(&enableGitReview, "enableGitReview", false, "Enable git review")
	flag.StringVar(&gitURL, "gitURL", "", "url of git repository")
	flag.StringVar(&gitPath, "gitPath", "", "local path of git repository")
	flag.StringVar(&subPath, "subPath", "", "relative path in git repository")
	flag.StringVar(&branch, "branch", k8sHost, "git branch")

	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	lw := &listener.ListenerWebhook{
		Logger:          logger,
		EnableGitReview: enableGitReview,
	}

	if enableGitReview {
		userName, _ := os.LookupEnv("GIT_USER_NAME")
		pwd, _ := os.LookupEnv("GIT_PASSWORD")

		auth := &http.BasicAuth{
			Username: userName,
			Password: pwd,
		}

		lw.GitConfig = listener.GitConfig{
			GitPath:   gitPath,
			SubPath:   subPath,
			GitBranch: branch,
			GitAuth:   auth,
		}

		if err := git.Clone(gitURL, gitPath, auth); err != nil {
			logger.Error(err, "failed to clone git repo", "url", gitURL, "path", gitPath)
			os.Exit(1)
		}

		if err := git.Checkout(gitPath, branch, logger); err != nil {
			logger.Error(err, "failed to checkout to git branch", "path", gitPath, "branch", branch)
			os.Exit(1)
		}

		if err := git.Pull(gitPath, branch); err != nil {
			logger.Error(err, "failed to pull remote repository", "path", gitPath)
			os.Exit(1)
		}
	}

	webhookServer := webhook.NewServer(webhook.Options{})
	webhookServer.Register("/listen", &admission.Webhook{Handler: lw, LogConstructor: func(base logr.Logger, req *admission.Request) logr.Logger {
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
