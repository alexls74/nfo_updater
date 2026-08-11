// internal/setup/system.go
package setup

import (
	"path/filepath"

	"nfo_updater/internal/config"
)

// askSystemPaths — секция размещения данных: база, логи, бэкапы.
//
// Устроена как один вопрос с уточнением, а не как три вопроса подряд.
// Подавляющему большинству нужен ответ "оставь как есть", и заставлять их
// трижды жать Enter незачем; но три пути остаются тремя параметрами конфига
// именно ради тех, кому нужна россыпь — база на быстром диске, бэкапы
// на большом. Второй вопрос существует ровно для этого случая.
//
// Пустое значение в конфиге означает "умолчание", поэтому согласие с
// умолчанием записывается пустыми строками, а не вычисленными путями.
// Это важно при перенастройке: человек, вернувший расположение к обычному,
// должен получить чистый конфиг, а не прибитый гвоздями путь, который
// перестанет следовать за программой, если раскладка когда-нибудь изменится.
func askSystemPaths(p *Prompt, values map[string]string) error {
	p.Section("DATA LOCATION")

	defaultRoot := config.DefaultDataDir()
	current := currentDataPaths(values, defaultRoot)

	if current.uniform {
		p.Note("The database, the logs and the backups are kept together in:")
		p.Note("  %s", current.root)
	} else {
		p.Note("The database, the logs and the backups are kept in:")
		p.Note("  database  %s", current.databaseDir)
		p.Note("  logs      %s", current.logs)
		p.Note("  backups   %s", current.backups)
	}

	keep, err := p.YesNo("Keep this arrangement?", true)
	if err != nil {
		return err
	}
	if keep {
		return nil
	}

	// Возврат к умолчанию отдельным вопросом.
	if !current.isDefault && defaultRoot != "" {
		useDefault, err := p.YesNo("Go back to the default location ("+defaultRoot+")?", false)
		if err != nil {
			return err
		}
		if useDefault {
			values["DATABASE_PATH"] = ""
			values["LOG_DIR"] = ""
			values["BACKUP_DIR"] = ""
			return nil
		}
	}

	root, _, err := askDir(p, "Directory for the database, the logs and the backups",
		current.root, checkDataDir, false)
	if err != nil {
		return err
	}

	// Дальше везде каталоги, включая базу: проверить на запись можно только
	// каталог, а имя файла программа дописывает сама.
	_, logs, backups := config.DataPathsUnder(root)
	databaseDir := root

	split, err := p.YesNo("Does any of the three need a different location?", false)
	if err != nil {
		return err
	}
	if split {
		if databaseDir, _, err = askDir(p, "Directory for the database file", databaseDir, checkDataDir, false); err != nil {
			return err
		}
		if logs, _, err = askDir(p, "Directory for the log files", logs, checkDataDir, false); err != nil {
			return err
		}
		if backups, _, err = askDir(p, "Directory for the backup archives", backups, checkDataDir, false); err != nil {
			return err
		}
	}

	// DATABASE_PATH — единственный из трёх, где в конфиге стоит путь к файлу,
	// а не к каталогу.
	databasePath, _, _ := config.DataPathsUnder(databaseDir)
	values["DATABASE_PATH"] = databasePath
	values["LOG_DIR"] = logs
	values["BACKUP_DIR"] = backups
	return nil
}

// dataPaths — текущее размещение данных в удобном для показа виде.
type dataPaths struct {
	databaseDir string
	logs        string
	backups     string

	// root — умолчание для вопроса о новом корне. Когда все три лежат
	// в одном дереве, это общий каталог; иначе каталог базы как наиболее
	// осмысленная из трёх отправных точек.
	root string
	// uniform — все три в одном каталоге, можно показать одной строкой.
	uniform bool
	// isDefault — конфиг не переопределяет ни одного из трёх путей.
	isDefault bool
}

// currentDataPaths разбирает, где сейчас лежат данные.
//
// Пустое значение в values означает "умолчание" — ровно та же трактовка,
// что и в конфиге. При неизвестном домашнем каталоге умолчания пусты и
// показываются как есть: врать про несуществующий путь нельзя, а разбираться
// с этим случаем всё равно придётся вызывающему.
func currentDataPaths(values map[string]string, defaultRoot string) dataPaths {
	defDB, defLogs, defBackups := config.DataPathsUnder(defaultRoot)

	databasePath := valueOr(values["DATABASE_PATH"], defDB)

	out := dataPaths{
		databaseDir: filepath.Dir(databasePath),
		logs:        valueOr(values["LOG_DIR"], defLogs),
		backups:     valueOr(values["BACKUP_DIR"], defBackups),
		isDefault: values["DATABASE_PATH"] == "" &&
			values["LOG_DIR"] == "" &&
			values["BACKUP_DIR"] == "",
	}

	// filepath.Dir("") даёт ".", а не пустую строку — подставлять точку
	// в вопрос о каталоге данных нельзя.
	if databasePath == "" {
		out.databaseDir = ""
	}

	out.uniform = out.databaseDir != "" &&
		out.databaseDir == out.logs &&
		out.databaseDir == out.backups
	out.root = out.databaseDir
	return out
}

// valueOr — значение или умолчание, если значение пусто. Ровно та же
// трактовка пустой строки, что у get() в config.fromRaw.
func valueOr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
