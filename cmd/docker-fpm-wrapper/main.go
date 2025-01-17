package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/code-tool/docker-fpm-wrapper/internal/applog"
	"github.com/code-tool/docker-fpm-wrapper/internal/breader"
	"github.com/code-tool/docker-fpm-wrapper/pkg/phpfpm"
)

func findFpmArgs() []string {
	doubleDashIndex := -1

	for i := range os.Args {
		if os.Args[i] == "--" {
			doubleDashIndex = i
			break
		}
	}
	if doubleDashIndex == -1 || doubleDashIndex+1 == len(os.Args) {
		return nil
	}

	return os.Args[doubleDashIndex+1:]
}

func main() {
	cfg, err := createConfig()
	if err != nil {
		fmt.Printf("Can't create app config: %v\n", err)
		os.Exit(1)
	}

	syncStderr := zapcore.Lock(os.Stderr)
	log, err := createLogger(cfg.LogEncoder, cfg.LogLevel, syncStderr)

	if cfg.FpmPath == "" {
		log.Error("php-fpm path not set")
		os.Exit(1)
	}

	errCh := make(chan error, 1)
	ctx, cancelCtx := context.WithCancel(context.Background())

	env := os.Environ()

	if cfg.WrapperSocket != "null" {
		env = append(env, fmt.Sprintf("FPM_WRAPPER_SOCK=unix://%s", cfg.WrapperSocket))
		sockDataListener := applog.NewSockDataListener(cfg.WrapperSocket, breader.NewPool(cfg.LineBufferSize), syncStderr, errCh)

		if err = sockDataListener.Start(); err != nil {
			log.Error("Can't start listen", zap.Error(err))
			os.Exit(1)
		}

		defer sockDataListener.Stop()
	}

	if cfg.WrapperPipe != "" {
		env = append(env, fmt.Sprintf("FPM_WRAPPER_PIPE=%s", cfg.WrapperPipe))

		wrapperPipe, err := createFIFOByPathCtx(ctx, cfg.WrapperPipe)
		if err != nil {
			log.Error("can't create pipe", zap.Error(err), zap.String("path", cfg.WrapperPipe))
			os.Exit(1)
		}

		go applog.NewPipeProxy(log.Named("pipe-proxy"), syncStderr).Proxy(wrapperPipe)
	}

	fpmConfig, err := phpfpm.ParseConfig(cfg.FpmConfigPath)
	if err != nil {
		log.Fatal("Can't parse fpm config", zap.Error(err))
		os.Exit(1)
	}

	if false == cfg.FpmNoErrlogProxy && fpmConfig.ErrorLog != "syslog" {
		if err := startErrLogProxy(ctx, log.Named("php-fpm"), fpmConfig.ErrorLog); err != nil {
			log.Error("can't start err_log proxy", zap.String("path", fpmConfig.ErrorLog), zap.Error(err))
			os.Exit(1)
		}
	}

	if false == cfg.FpmNoSlowlogProxy {
		if err = startSlowlogProxies(ctx, log.Named("php-fpm"), fpmConfig.Pools); err != nil {
			log.Error("Can't start slowlog proxies", zap.Error(err))
			os.Exit(1)
		}
	}

	fpmProcess := phpfpm.
		NewProcess(log, cfg.FpmPath, cfg.FpmConfigPath, os.Stdout, syncStderr, cfg.ShutdownDelay, env, findFpmArgs()...)

	if err = fpmProcess.Start(); err != nil {
		log.Fatal("Can't start php-fpm", zap.Error(err))
		os.Exit(1)
	}

	prometheus.MustRegister(
		phpfpm.NewPromCollector(log.Named("prom-collector"), phpfpm.NewPromMetrics(), fpmConfig.Pools),
	)

	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGUSR1, syscall.SIGUSR2)
	go fpmProcess.HandleSignal(signalCh)

	fpmExitCodeCh := make(chan int, 1)
	go func() {
		fpmExitCodeCh <- fpmProcess.Wait(errCh)
	}()

	http.Handle(cfg.MetricsPath, promhttp.Handler())
	go func() {
		errCh <- http.ListenAndServe(cfg.Listen, nil)
	}()

	for {
		select {
		case err := <-errCh:
			cancelCtx()
			if err != nil {
				log.Fatal("", zap.Error(err))
			}
		case exitCode := <-fpmExitCodeCh:
			cancelCtx()
			os.Exit(exitCode)
		}
	}
}
