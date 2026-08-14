// internal/daemon/daemon.go
package daemon

import (
	"context"
	"os"
	"os/signal"
	"sync"
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
// нельзя.
type Daemon struct {
	run    RunFunc
	reload ReloadFunc
	logger *logging.Logger

	running       atomic.Bool
	reloadPending atomic.Bool

	// forced учитывает внеплановые прогоны по SIGUSR1. Они запускаются
	// в отдельных горутинах, иначе прогон, длящийся полчаса, на все эти
	// полчаса лишил бы демона слуха: ни остановиться, ни перечитать
	// конфиг он бы не смог.
	//
	// Плата за это — необходимость их дождаться при выходе. Прогоны
	// по расписанию идут синхронно и покрыты ожиданием в doDaemon,
	// а внеплановые до появления этого счётчика не были покрыты ничем:
	// процесс мог завершиться, пока прогон ещё сворачивается, — то есть
	// с недоупакованным бэкапом и незакрытой базой.
	forced sync.WaitGroup
}

func New(run RunFunc, reload ReloadFunc, logger *logging.Logger) *Daemon {
	return &Daemon{run: run, reload: reload, logger: logger}
}

// Serve слушает сигналы, специфичные для режима демона, и возвращается,
// когда отменён ctx.
//
// SIGTERM и SIGINT здесь НЕ перехватываются: остановка — общее для всех
// режимов поведение, и её обрабатывает main, отменяя ctx. Иначе одноразовый
// прогон оставался бы беззащитен перед Ctrl+C, а незакрытая по такому
// выходу база оставляла бы после себя -wal и -shm.
//
// Демону остаются два сигнала, осмысленные только для него: SIGUSR1
// (внеплановый прогон) и SIGHUP (перезагрузка конфига). В одноразовом
// режиме SIGHUP смысла не имеет — конфиг там читается один раз, — и main
// трактует его как обычную остановку, что для оборванного терминала
// и есть правильное поведение.
func (d *Daemon) Serve(ctx context.Context) error {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGUSR1, syscall.SIGHUP)
	defer signal.Stop(sigCh)

	// Ожидание внеплановых прогонов регистрируется ПОСЛЕ signal.Stop,
	// то есть отработает раньше него: пока мы ждём, перехват сигналов
	// ещё стоит. Иначе второй SIGTERM во время ожидания убил бы процесс
	// поведением по умолчанию — ровно то, от чего мы прогон и защищаем.
	// Осознанный немедленный выход по второму сигналу остаётся возможен,
	// но решает это main, а не диспозиция по умолчанию.
	defer d.forced.Wait()

	for {
		select {
		case <-ctx.Done():
			return nil
		case sig := <-sigCh:
			switch sig {
			case syscall.SIGUSR1:
				d.logger.Event("[SIGNAL] SIGUSR1 received, triggering forced run")
				d.forced.Add(1)
				go func() {
					defer d.forced.Done()
					d.TriggerRun(ctx)
				}()
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
