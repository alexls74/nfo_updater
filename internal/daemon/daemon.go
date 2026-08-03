// internal/daemon/daemon.go
package daemon

import (
	"context"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"

	"nfo_updater/internal/logging"
)

// RunFunc выполняет один полный проход (захват lock-файла, обход директорий,
// запрос рейтингов, обновление .nfo-файлов). Должен уважать отмену ctx.
type RunFunc func(ctx context.Context) error

// ReloadFunc перечитывает конфиг и пересобирает всё, что от него зависит.
// Возвращает ошибку, если новый конфиг невалиден — старое состояние
// остаётся действующим.
type ReloadFunc func() error

// Daemon держит "загрузочный" логгер, а не логгер прогона: файл лога
// открывается и закрывается внутри самого прогона (см. Runner.Run), поэтому
// сообщения демона — сигналы, reload, отклонённый запуск — писать в него
// нельзя, их время наступает до или после существования этого файла.
type Daemon struct {
	run    RunFunc
	reload ReloadFunc
	logger *logging.Logger

	running       atomic.Bool
	reloadPending atomic.Bool
}

func New(run RunFunc, reload ReloadFunc, logger *logging.Logger) *Daemon {
	return &Daemon{run: run, reload: reload, logger: logger}
}

func (d *Daemon) Serve(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT, syscall.SIGUSR1, syscall.SIGHUP)

	for {
		select {
		case <-ctx.Done():
			return nil
		case sig := <-sigCh:
			switch sig {
			case syscall.SIGTERM, syscall.SIGINT:
				d.logger.Event("[SHUTDOWN] received %s, stopping", sig)
				cancel()
			case syscall.SIGUSR1:
				d.logger.Event("[SIGNAL] SIGUSR1 received, triggering forced run")
				go d.TriggerRun(ctx)
			case syscall.SIGHUP:
				d.handleReloadSignal()
			}
		}
	}
}

func (d *Daemon) handleReloadSignal() {
	if d.running.Load() {
		d.reloadPending.Store(true)
		d.logger.Event("[RELOAD_DEFERRED] SIGHUP received while a run is in progress, will apply after it finishes")
		return
	}
	d.applyReload()
}

func (d *Daemon) applyReload() {
	d.logger.Event("[RELOAD_START] reloading config")
	if err := d.reload(); err != nil {
		d.logger.Event("[RELOAD_ERROR] %v — continuing with the previous config", err)
		return
	}
	d.logger.Event("[RELOAD_DONE] config reloaded successfully")
}

// TriggerRun запускает прогон, если другой не идёт прямо сейчас.
//
// Начало и успешное завершение прогона демон НЕ логирует: это делает сам
// прогон ([RUN_START] с версией и сборкой в начале, итоговая сводка в конце),
// причём в файл лога прогона, где этим сообщениям и место. Демону остаются
// только те два случая, о которых прогон сообщить не может: его вообще
// не запустили, или он завершился ошибкой.
func (d *Daemon) TriggerRun(ctx context.Context) {
	if !d.running.CompareAndSwap(false, true) {
		d.logger.Event("[RUN_REJECTED] a run was requested while another run is already in progress")
		return
	}
	defer func() {
		d.running.Store(false)
		if d.reloadPending.CompareAndSwap(true, false) {
			d.applyReload()
		}
	}()

	if err := d.run(ctx); err != nil {
		d.logger.Event("[RUN_ERROR] %v", err)
	}
}
