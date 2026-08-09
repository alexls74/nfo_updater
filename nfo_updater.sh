#!/bin/sh
#
# nfo_updater.sh — установка, обновление и удаление NFO Updater.
#
# Скрипт не остаётся на диске: приходит по каналу, отрабатывает, исчезает.
#
#   wget -qO- https://raw.githubusercontent.com/alexls74/nfo_updater/main/nfo_updater.sh | sh -s -- install
#
# Разделение обязанностей жёсткое. Скрипт делает ровно то, чего программа
# про себя знать не может: скачивает релиз, сверяет контрольную сумму,
# кладёт файлы и разговаривает с systemd. Всё остальное — мастер настройки,
# текст юнита, проверка ключей по сети, правила путей медиатеки — делает сама
# программа. Переписывать это на POSIX sh значит получить вторую, худшую
# реализацию каждого пункта.
#
# Скрипт запускается по каналу, поэтому его собственный stdin — это тело
# скрипта, а не терминал. Всё, что нужно спросить, спрашивается через
# /dev/tty; мастер настройки внутри бинарника поступает так же и по той же
# причине.

set -eu

REPO='alexls74/nfo_updater'
BIN_NAME='nfo_updater'
UNIT_NAME='nfo_updater.service'
UNIT_DIR='/etc/systemd/system'
SYSTEM_BIN_DIR='/usr/local/bin'
USER_BIN_DIR="${HOME:-}/.local/bin"

# Коды возврата бинарника. Держать в согласии с internal/exitcode/exitcode.go:
# числа входят во внешний интерфейс программы именно ради этого файла.
EXIT_SETUP_ABORTED=4
EXIT_SETUP_SERVICE=10

# Заполняются по ходу дела.
DL=''         # curl или wget
SHA=''        # команда подсчёта sha256
SUDO=''       # 'sudo' или пусто, см. need_sudo_for
WORKDIR=''    # временный каталог, снимается по trap
REL_BASE=''    # адрес, откуда берётся релиз
REL_ARCHIVE='' # имя файла архива
REL_VERSION='' # версия релиза
REL_BIN=''     # путь к распакованному бинарнику

# ---------------------------------------------------------------------------
# Вывод и мелкие помощники
# ---------------------------------------------------------------------------

say()  { printf '%s\n' "$*"; }
warn() { printf '%s\n' "$*" >&2; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }

# have_tty проверяет терминал попыткой открытия, а не тестом -r: файл
# устройства существует и по правам читаем всегда, а вот открыть его без
# управляющего терминала нельзя — именно так выглядит запуск из cron,
# из systemd или по ssh с -T.
# Открытие делается в подоболочке. Двоеточие — специальный встроенный
# оператор, а ошибка перенаправления на таком по POSIX завершает весь
# неинтерактивный шелл; dash так и поступает, и скрипт умирал прямо здесь
# с кодом 2 вместо честного "терминала нет".
have_tty() { (: </dev/tty) 2>/dev/null; }

# confirm читает ответ из /dev/tty, а не из stdin: stdin занят телом скрипта.
# Отсутствие терминала — отказ, а не молчаливое согласие: вопрос задаётся
# только там, где последствия того стоят.
confirm() {
	have_tty || return 1
	printf '%s [y/N] ' "$1" >/dev/tty
	read -r _ans </dev/tty || return 1
	case "$_ans" in
	[yY] | [yY][eE][sS]) return 0 ;;
	*) return 1 ;;
	esac
}

require_tty() {
	have_tty || die "this command asks questions and needs a terminal, but none is available"
}

# ---------------------------------------------------------------------------
# Проверки окружения
# ---------------------------------------------------------------------------

# Релиз собирается только под linux/amd64 (см. Makefile). Сказать об этом
# сразу честнее, чем дать скачать архив и упасть на "cannot execute binary
# file": на NAS с ARM это самый вероятный исход.
check_platform() {
	[ "$(uname -s)" = 'Linux' ] || die "NFO Updater is built for Linux only"
	case "$(uname -m)" in
	x86_64 | amd64) ;;
	*) die "NFO Updater is built for x86_64 only, this machine is $(uname -m)" ;;
	esac
}

detect_tools() {
	if command -v curl >/dev/null 2>&1; then
		DL='curl'
	elif command -v wget >/dev/null 2>&1; then
		DL='wget'
	else
		die "neither curl nor wget is available, cannot download anything"
	fi

	command -v tar >/dev/null 2>&1 || die "tar is not available"

	if command -v sha256sum >/dev/null 2>&1; then
		SHA='sha256sum'
	elif command -v shasum >/dev/null 2>&1; then
		SHA='shasum -a 256'
	else
		die "no sha256 tool found, the download cannot be verified"
	fi
}

# Утечка root. Под sudo $HOME становится /root: конфиг с ключами API уехал бы
# в /root/.config, где обычный пользователь его не найдёт и прав на него
# не имеет, а служба получила бы User=root. Поведение sudo с HOME разнится
# между дистрибутивами, угадывать нельзя. Бинарник отказывается выполнять
# --setup и --print-unit при том же условии.
#
# Настоящий вход под root (SUDO_USER пуст) — другой случай: на NAS
# с единственной учётной записью это нормальный режим, запрещать его нельзя.
check_root_leak() {
	[ "$(id -u)" -eq 0 ] || return 0

	if [ -n "${SUDO_USER:-}" ]; then
		die "do not run this under sudo.
The configuration belongs to your own user; under sudo it would end up in
root's home directory instead. Run the command as yourself — the password
is asked for automatically, and only for the steps that need it."
	fi

	warn "You are running as root. Everything will be installed for root:
the configuration, the database and the backups will live in root's home
directory. That is fine on an appliance with a single account, and probably
not what you want otherwise."
	confirm "Continue as root?" || {
		say "Aborted."
		exit 0
	}
}

# need_sudo_for решает, нужен ли sudo для записи в каталог. Проверяется
# фактическая запись, а не имя каталога: на Debian и Ubuntu /usr/local/bin
# принадлежит root:staff с правом записи для группы, но в staff по умолчанию
# никто не входит — закладываться нельзя ни на то, ни на другое.
need_sudo_for() {
	SUDO=''
	[ "$(id -u)" -eq 0 ] && return 0
	[ -w "$1" ] && return 0
	command -v sudo >/dev/null 2>&1 || die "$1 is not writable and sudo is not available"
	SUDO='sudo'
	return 0
}

# Пароль спрашивается один раз и заранее, отдельной строкой с объяснением.
# sudo кэширует его на пятнадцать минут, и всё, что идёт следом, проходит
# молча — а мастер настройки к этому моменту уже пройден, так что кэш
# не успевает истечь, пока человек ходит за ключами API.
prime_sudo() {
	[ -n "$SUDO" ] || return 0
	say ""
	say "The next steps need administrator rights: putting the program into"
	say "$SYSTEM_BIN_DIR and registering the service."
	sudo -v || die "could not obtain administrator rights"
}

# ---------------------------------------------------------------------------
# Временный каталог
#
# В домашнем каталоге, а не в /tmp: на затянутых системах /tmp монтируют
# с noexec, а нам нужно запустить оттуда мастер настройки.
# ---------------------------------------------------------------------------

cleanup() {
	[ -n "$WORKDIR" ] && rm -rf "$WORKDIR"
	return 0
}

make_workdir() {
	[ -n "${HOME:-}" ] || die "HOME is not set, cannot pick a working directory"
	[ -d "$HOME" ] || die "HOME points at $HOME, which is not a directory"
	WORKDIR=$(mktemp -d "$HOME/.nfo_updater_setup.XXXXXX") || die "cannot create a working directory in $HOME"
	trap cleanup EXIT
	trap 'cleanup; exit 130' INT
	trap 'cleanup; exit 143' TERM
}

# ---------------------------------------------------------------------------
# Загрузка релиза
# ---------------------------------------------------------------------------

download() { # url dest
	case "$DL" in
	curl) curl -fsSL -o "$2" "$1" ;;
	wget) wget -q -O "$2" "$1" ;;
	esac
}

# Загрузка разделена на два шага, потому что версия релиза известна раньше
# самого релиза: имя архива содержит её, а имя архива написано в checksums.txt,
# который лежит под фиксированным адресом. Значит update может сравнить версии
# и разойтись, не выкачивая несколько мегабайт впустую.
#
# Так же обходится api.github.com, который для неаутентифицированных запросов
# быстро упирается в лимит.

# resolve_release [версия] — выясняет, что за релиз нам предлагают.
# Выставляет REL_VERSION, REL_ARCHIVE и REL_BASE.
resolve_release() {
	_want="${1:-}"
	if [ -n "$_want" ]; then
		REL_BASE="https://github.com/$REPO/releases/download/v$_want"
	else
		REL_BASE="https://github.com/$REPO/releases/latest/download"
	fi

	say "Fetching the release information..."
	download "$REL_BASE/checksums.txt" "$WORKDIR/checksums.txt" ||
		die "cannot download the release. Check the network, and if you asked for a specific version, check that it exists at https://github.com/$REPO/releases"

	REL_ARCHIVE=$(awk 'NR == 1 { print $2 }' "$WORKDIR/checksums.txt")
	[ -n "$REL_ARCHIVE" ] || die "the checksum file is empty or malformed"

	REL_VERSION=$(printf '%s\n' "$REL_ARCHIVE" |
		sed -n 's/^'"$BIN_NAME"'_v\(.*\)_linux_amd64\.tar\.gz$/\1/p')
	[ -n "$REL_VERSION" ] || die "cannot tell the version from the archive name: $REL_ARCHIVE"
}

# fetch_archive — качает, сверяет и распаковывает то, что нашла resolve_release.
fetch_archive() {
	say "Downloading NFO Updater $REL_VERSION..."
	download "$REL_BASE/$REL_ARCHIVE" "$WORKDIR/$REL_ARCHIVE" || die "cannot download $REL_ARCHIVE"

	# Сверка обязательна и молчаливой быть не должна: это единственная защита
	# от подменённого или недокачанного архива, который дальше будет запущен.
	(cd "$WORKDIR" && $SHA -c checksums.txt >/dev/null 2>&1) ||
		die "checksum mismatch: the downloaded archive does not match checksums.txt. Nothing has been installed."

	mkdir -p "$WORKDIR/pack"
	tar -xzf "$WORKDIR/$REL_ARCHIVE" -C "$WORKDIR/pack" || die "cannot unpack $REL_ARCHIVE"

	REL_BIN="$WORKDIR/pack/$BIN_NAME"
	[ -f "$REL_BIN" ] || die "the archive does not contain $BIN_NAME"
	chmod +x "$REL_BIN"
}

# ---------------------------------------------------------------------------
# Установленная копия
# ---------------------------------------------------------------------------

# find_installed печатает путь к установленному бинарнику или молчит и
# возвращает 1. Известные места проверяются раньше PATH: в PATH может
# оказаться совсем другая сборка, положенная руками.
find_installed() {
	for _d in "$SYSTEM_BIN_DIR" "$USER_BIN_DIR"; do
		if [ -n "$_d" ] && [ -x "$_d/$BIN_NAME" ]; then
			printf '%s\n' "$_d/$BIN_NAME"
			return 0
		fi
	done
	if _p=$(command -v "$BIN_NAME" 2>/dev/null) && [ -n "$_p" ]; then
		printf '%s\n' "$_p"
		return 0
	fi
	return 1
}

installed_version() { # путь к бинарнику
	"$1" --version 2>/dev/null | awk '{ print $2 }'
}

# place_binary кладёт бинарник через переименование внутри каталога
# назначения, а не записью поверх.
#
# Причина не косметическая. Запись поверх работающего исполняемого файла даёт
# ETXTBSY, а прогон в этот момент вполне может идти. Переименование внутри
# одного каталога атомарно, и уже работающий процесс продолжает жить со своим
# старым inode до конца прогона — то есть обновление никого не обрывает
# на середине и не оставляет бэкапы неупакованными.
place_binary() { # src destdir
	_tmp="$2/.$BIN_NAME.new.$$"
	$SUDO install -m 0755 "$1" "$_tmp" || die "cannot write to $2"
	$SUDO mv -f "$_tmp" "$2/$BIN_NAME" || die "cannot put the binary into $2"
}

# ---------------------------------------------------------------------------
# Служба
# ---------------------------------------------------------------------------

# Проверяется не наличие команды, а то, что systemd действительно работает.
# systemctl может лежать в PATH на машине, где init совсем другой: WSL,
# контейнер, система с systemd, поставленным пакетом, но не запущенным.
# Каталог /run/systemd/system создаётся только работающим systemd — это тот же
# признак, по которому его определяют sd_booted() и maintainer-скрипты Debian.
have_systemd() { command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; }
unit_installed() { [ -f "$UNIT_DIR/$UNIT_NAME" ]; }

service_active() {
	have_systemd || return 1
	systemctl is-active --quiet "$UNIT_NAME" 2>/dev/null
}

# write_unit <бинарник-генератор> <путь-к-установленному-бинарнику>
#
# Текст юнита печатает сама программа: в нём её собственные пути, имя
# пользователя и каталоги медиатеки, которые она читает из конфига. Путь
# к установленному бинарнику передаётся аргументом, потому что при установке
# генератор ещё лежит во временном каталоге.
#
# Файл переписывается только при расхождении: вывод --print-unit
# детерминирован, поэтому совпадение означает, что менять нечего, и лишний
# daemon-reload не нужен.
write_unit() {
	_gen="$1"
	_target="$2"
	_new="$WORKDIR/$UNIT_NAME"

	if ! "$_gen" --print-unit "$_target" >"$_new"; then
		warn "the program refused to generate the unit file (see the message above)"
		return 1
	fi

	if unit_installed && cmp -s "$_new" "$UNIT_DIR/$UNIT_NAME"; then
		return 0
	fi
	$SUDO install -m 0644 "$_new" "$UNIT_DIR/$UNIT_NAME" || {
		warn "cannot write $UNIT_DIR/$UNIT_NAME"
		return 1
	}
	$SUDO systemctl daemon-reload
}

# Файл юнита снимается даже там, где systemd не работает: он мог остаться
# от установки на машине, которую с тех пор перевели на другой init, и не
# убрать его значит оставить мусор в /etc.
remove_unit() {
	unit_installed || return 0
	$SUDO systemctl disable --now "$UNIT_NAME" >/dev/null 2>&1 || true
	$SUDO rm -f "$UNIT_DIR/$UNIT_NAME"
	$SUDO systemctl daemon-reload >/dev/null 2>&1 || true
}

# ---------------------------------------------------------------------------
# Команды
# ---------------------------------------------------------------------------

cmd_install() { # user_mode
	_user_mode="$1"

	check_platform
	detect_tools
	check_root_leak
	require_tty

	if _found=$(find_installed); then
		say "NFO Updater is already installed at $_found ($(installed_version "$_found"))."
		say "Use 'update' to upgrade it, or 'configure' to change its settings."
		exit 0
	fi

	if [ "$_user_mode" = 'yes' ]; then
		_dir="$USER_BIN_DIR"
		[ -n "${HOME:-}" ] || die "HOME is not set, --user has nowhere to install to"
		mkdir -p "$_dir"
	else
		_dir="$SYSTEM_BIN_DIR"
		[ -d "$_dir" ] || die "$_dir does not exist"
	fi

	make_workdir
	resolve_release ''
	fetch_archive

	# Мастер идёт ДО всего, что требует пароля: человек проходит его небыстро,
	# он ходит за тремя ключами API по сайтам, и кэш sudo за это время истёк
	# бы. Мастер ничего не пишет на диск до последнего вопроса, так что выход
	# на любом шаге оставляет систему нетронутой.
	set +e
	"$REL_BIN" --setup
	_rc=$?
	set -e

	case "$_rc" in
	0) _want_service='no' ;;
	"$EXIT_SETUP_SERVICE") _want_service='yes' ;;
	"$EXIT_SETUP_ABORTED")
		# О прерывании человеку уже сказал сам мастер, на том же экране,
		# где шёл разговор. Повторять незачем — добавляем только то, чего
		# мастер знать не может: что установка тоже не состоялась.
		say "Nothing has been installed."
		exit 0
		;;
	*) die "setup did not finish (exit code $_rc). Nothing has been installed." ;;
	esac

	if [ "$_want_service" = 'yes' ]; then
		if [ "$_user_mode" = 'yes' ]; then
			warn "Note: --user installs the program for you alone and does not register a
system service. The schedule has been saved in the configuration, but nothing
will start NFO Updater on its own — run it yourself when you want a pass."
			_want_service='no'
		elif ! have_systemd; then
			warn "Note: systemd was not found on this machine, so no service has been
registered. The schedule has been saved in the configuration; start NFO Updater
with -d from whatever supervises services here."
			_want_service='no'
		fi
	fi

	need_sudo_for "$_dir"

	# Юнит генерируется ДО получения прав: генерация root не требует, а вот
	# читать конфиг из-под sudo нельзя — он лежит в домашнем каталоге
	# обычного пользователя.
	_unit_text=''
	if [ "$_want_service" = 'yes' ]; then
		need_sudo_for "$UNIT_DIR"
		_unit_text='yes'
	fi

	prime_sudo
	place_binary "$REL_BIN" "$_dir"

	# Сбой регистрации службы не отменяет установки и не должен выглядеть как
	# провал: программа к этому моменту уже лежит на месте и работоспособна.
	# Говорим, что именно не получилось, и чем это чинится.
	if [ -n "$_unit_text" ]; then
		if write_unit "$_dir/$BIN_NAME" "$_dir/$BIN_NAME" &&
			$SUDO systemctl enable --now "$UNIT_NAME"; then
			:
		else
			_unit_text=''
			warn "The program is installed, but the service could not be registered.
Once that is sorted out, add the service with this script's 'service on'."
		fi
	fi

	say ""
	say "NFO Updater $REL_VERSION is installed at $_dir/$BIN_NAME"
	if [ -n "$_unit_text" ]; then
		say "The service is enabled and running; it will update the library on the"
		say "schedule you chose."
		say "  systemctl status $UNIT_NAME     see what it is doing"
		say "  journalctl -u $UNIT_NAME -f     follow the log"
	else
		say "Start a pass whenever you want one:"
		say "  $BIN_NAME"
		say "To have it run on a schedule later: this script's 'service on'."
	fi
}

cmd_update() { # user_mode want_version
	_user_mode="$1"
	_want="${2:-}"

	check_platform
	detect_tools
	check_root_leak

	_bin=$(find_installed) || die "NFO Updater is not installed. Run 'install' first."
	_dir=$(dirname "$_bin")
	_cur=$(installed_version "$_bin")

	make_workdir
	resolve_release "$_want"

	# Сравнение до загрузки архива: версия уже известна из checksums.txt,
	# и качать несколько мегабайт ради того, чтобы выбросить, незачем.
	if [ "$_cur" = "$REL_VERSION" ]; then
		say "Already at $REL_VERSION, nothing to do."
		exit 0
	fi
	say "Updating $_cur -> $REL_VERSION"
	fetch_archive

	need_sudo_for "$_dir"
	if unit_installed; then need_sudo_for "$UNIT_DIR"; fi
	prime_sudo

	# Остановка службы — это и есть ожидание прогона. Демон по SIGTERM
	# отменяет контекст, прогон доупаковывает бэкапы уже изменённых файлов
	# и выходит, а systemd ждёт его столько, сколько указано
	# в TimeoutStopSec. Отдельной логики "подождать или отказаться" здесь
	# не нужно.
	#
	# Для установки без службы ждать тоже нечего: бинарник кладётся
	# переименованием, и запущенный вручную прогон доживает со старым
	# inode до конца.
	_was_active='no'
	if service_active; then
		_was_active='yes'
		say "Stopping the service; if a pass is running, systemd waits for it to finish."
		$SUDO systemctl stop "$UNIT_NAME"
	fi

	place_binary "$REL_BIN" "$_dir"

	# Юнит между версиями мог измениться. Генерируем уже установленным
	# бинарником — новым, — и переписываем только при расхождении.
	# Бинарник уже заменён, поэтому неудача с юнитом обновление не отменяет:
	# сказать о ней надо, а откатывать нечего.
	if unit_installed; then
		write_unit "$_dir/$BIN_NAME" "$_dir/$BIN_NAME" ||
			warn "The new binary is in place, but the unit file could not be refreshed."
	fi

	if [ "$_was_active" = 'yes' ]; then
		$SUDO systemctl start "$UNIT_NAME"
		say "Service restarted."
	fi

	say "NFO Updater $REL_VERSION is installed at $_dir/$BIN_NAME"
	[ "$_user_mode" = 'yes' ] && say "(--user has no effect on update: the program is replaced where it already is.)"
	return 0
}

cmd_configure() {
	check_root_leak
	require_tty

	_bin=$(find_installed) || die "NFO Updater is not installed. Run 'install' first."
	_dir=$(dirname "$_bin")

	set +e
	"$_bin" --setup
	_rc=$?
	set -e

	case "$_rc" in
	0) _want_service='no' ;;
	"$EXIT_SETUP_SERVICE") _want_service='yes' ;;
	"$EXIT_SETUP_ABORTED")
		exit 0
		;;
	*) die "setup did not finish (exit code $_rc)" ;;
	esac

	# Настройка приводит систему в соответствие с ответом. Отказ от работы
	# по расписанию убирает службу, согласие — заводит: иначе человек,
	# выключивший расписание, получил бы работающую службу, которая по
	# пустому SCHEDULE молча делает вид, что так и надо.
	if [ "$_want_service" = 'yes' ]; then
		have_systemd || die "systemd was not found, cannot register a service"
		need_sudo_for "$UNIT_DIR"
		prime_sudo
		write_unit "$_bin" "$_dir/$BIN_NAME" || die "the service could not be registered"
		$SUDO systemctl enable --now "$UNIT_NAME"
		say "Configuration saved; the service is enabled and running."
	elif unit_installed; then
		need_sudo_for "$UNIT_DIR"
		prime_sudo
		remove_unit
		say "Configuration saved; the service has been removed, the program stays."
	else
		say "Configuration saved."
	fi
}

cmd_service_on() {
	check_root_leak
	have_systemd || die "systemd was not found on this machine"

	_bin=$(find_installed) || die "NFO Updater is not installed. Run 'install' first."
	_dir=$(dirname "$_bin")

	if unit_installed && systemctl is-enabled --quiet "$UNIT_NAME" 2>/dev/null; then
		say "The service is already installed and enabled."
		exit 0
	fi

	# Мастер не запускается: режим установки — свойство системы, а не
	# конфигурации. Юнит собирается из того, что уже записано на диске.
	need_sudo_for "$UNIT_DIR"
	make_workdir
	prime_sudo
	write_unit "$_bin" "$_dir/$BIN_NAME" || die "the service could not be registered"
	$SUDO systemctl enable --now "$UNIT_NAME"
	say "The service is enabled and running."
}

cmd_service_off() {
	check_root_leak

	if ! unit_installed; then
		say "The service is not installed; there is nothing to switch off."
		exit 0
	fi
	need_sudo_for "$UNIT_DIR"
	prime_sudo
	remove_unit
	say "The service has been removed. The program and its configuration are untouched."
}

cmd_remove() {
	check_root_leak

	_bin=$(find_installed) || {
		say "NFO Updater is not installed."
		exit 0
	}

	# Пути спрашиваются ДО удаления: после него спросить будет некого,
	# а сказать человеку, где остались его база, логи и бэкапы, обязательно.
	_info=$("$_bin" -v 2>/dev/null || true)

	say "This will remove $_bin and the service, if there is one."
	say "The configuration file, the database, the logs and the backups will be left"
	say "where they are."
	confirm "Remove NFO Updater?" || {
		say "Cancelled."
		exit 0
	}

	if unit_installed; then need_sudo_for "$UNIT_DIR"; fi
	need_sudo_for "$(dirname "$_bin")"
	prime_sudo

	remove_unit
	$SUDO rm -f "$_bin"

	say ""
	say "Removed."
	if [ -n "$_info" ]; then
		say ""
		say "Your data has been left alone. It lives here:"
		printf '%s\n' "$_info"
		say ""
		say "Delete those directories by hand if you want them gone."
	fi
}

cmd_status() {
	if ! _bin=$(find_installed); then
		say "NFO Updater is not installed."
		exit 0
	fi

	say "Installed at: $_bin"
	say ""
	"$_bin" -v || true
	say ""

	if ! have_systemd; then
		say "Service:      systemd is not available on this machine"
		return 0
	fi
	if ! unit_installed; then
		say "Service:      not installed (on-demand use)"
		return 0
	fi
	say "Service:      $UNIT_DIR/$UNIT_NAME"
	say "  enabled:    $(systemctl is-enabled "$UNIT_NAME" 2>/dev/null || echo unknown)"
	say "  active:     $(systemctl is-active "$UNIT_NAME" 2>/dev/null || echo unknown)"
}

cmd_help() {
	cat <<EOF
NFO Updater installer

  wget -qO- https://raw.githubusercontent.com/$REPO/main/nfo_updater.sh | sh -s -- COMMAND

Commands:

  (none)         Show a menu.
  install        Download, set up and install. The setup wizard asks whether
                 the library should be updated on a schedule and registers a
                 service if it should.
  service on     Register the service for an already installed program,
                 without going through the wizard again.
  service off    Remove the service. The program and its settings stay.
  configure      Go through the setup wizard again.
  update         Update to the latest release. 'update 1.2.0' goes to a
                 specific one.
  remove         Remove the program and the service. Configuration, database,
                 logs and backups are left alone.
  status         Show what is installed, where, and what the service is doing.
  help           This text.

Options:

  --user         For 'install': put the program in ~/.local/bin instead of
                 $SYSTEM_BIN_DIR, without asking for a password. No service
                 is registered in this mode. Meant for machines where you have
                 no administrator rights.
EOF
}

menu() {
	require_tty
	cat <<EOF
NFO Updater

  1  install      set up and install
  2  update       update to the latest release
  3  configure    go through the setup wizard again
  4  service on   register the service
  5  service off  remove the service, keep the program
  6  status       what is installed and what it is doing
  7  remove       remove the program
  q  quit

EOF
	printf 'Choose: ' >/dev/tty
	read -r _choice </dev/tty || exit 0
	_choice=$(printf '%s' "$_choice" | tr -d ' \t')
	case "$_choice" in
	1) cmd_install 'no' ;;
	2) cmd_update 'no' '' ;;
	3) cmd_configure ;;
	4) cmd_service_on ;;
	5) cmd_service_off ;;
	6) cmd_status ;;
	7) cmd_remove ;;
	q | Q | '') exit 0 ;;
	*) die "no such choice: $_choice" ;;
	esac
}

# ---------------------------------------------------------------------------
# Разбор аргументов
# ---------------------------------------------------------------------------

main() {
	_user_mode='no'
	_words=''

	for _a in "$@"; do
		case "$_a" in
		--user) _user_mode='yes' ;;
		-h | --help) _words="$_words help" ;;
		-*) die "unknown option: $_a (try 'help')" ;;
		# Слова команд и номера версий пробелов не содержат, поэтому
		# накопление в строке безопасно и обходится без массивов,
		# которых в POSIX sh нет.
		*) _words="$_words $_a" ;;
		esac
	done

	# shellcheck disable=SC2086
	set -- $_words

	if [ "$_user_mode" = 'yes' ]; then
		case "${1:-}" in
		install | update) ;;
		*) die "--user only applies to 'install' and 'update'" ;;
		esac
	fi

	case "${1:-}" in
	'') menu ;;
	install) cmd_install "$_user_mode" ;;
	update) cmd_update "$_user_mode" "${2:-}" ;;
	configure) cmd_configure ;;
	remove) cmd_remove ;;
	status) cmd_status ;;
	service)
		case "${2:-}" in
		on) cmd_service_on ;;
		off) cmd_service_off ;;
		'') die "'service' needs 'on' or 'off'" ;;
		*) die "unknown argument for 'service': $2" ;;
		esac
		;;
	help) cmd_help ;;
	*) die "unknown command: $1 (try 'help')" ;;
	esac
}

main "$@"
