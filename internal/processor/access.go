// internal/processor/access.go
package processor

import (
	"fmt"
	"os"
	"syscall"
)

// Проверки прав на запись. Нужны по двум причинам, и обе про экономию:
// бессмысленно тратить запрос к API (а значит, суточную квоту) ради файла,
// который мы всё равно не сможем перезаписать, и бессмысленно обходить
// корень, недоступный целиком.
//
// Используется syscall.Access, а не разбор os.FileInfo().Mode(): режим
// говорит только о битах прав, но ничего не знает ни о владельце, ни
// о ACL, ни о смонтированном read-only разделе. Access задаёт вопрос ядру
// и получает ответ с учётом всего сразу.
//
// Одна известная особенность: Access проверяет РЕАЛЬНЫЙ uid, а не
// эффективный. Для нас это безразлично — демон работает под обычным
// пользователем без setuid, у него они совпадают. И под root проверка
// всегда проходит: root может писать куда угодно, ядро отвечает честно.

// checkWritableFile сообщает, сможем ли мы перезаписать существующий файл.
// Запись идёт напрямую через os.WriteFile, без временного файла и rename,
// поэтому проверять права на каталог не нужно — только на сам файл.
func checkWritableFile(path string) error {
	if err := syscall.Access(path, unixWOK); err != nil {
		return fmt.Errorf("no write permission: %w", err)
	}
	return nil
}

// checkWritableDir проверяет, что каталог существует, является каталогом
// и в него можно зайти. Права на запись В САМ каталог не требуются: мы
// не создаём и не удаляем в медиатеке файлы, только перезаписываем
// существующие. Поэтому здесь R_OK|X_OK — прочитать список и войти внутрь.
func checkWritableDir(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("not a directory")
	}
	if err := syscall.Access(path, unixROK|unixXOK); err != nil {
		return fmt.Errorf("directory is not accessible: %w", err)
	}
	return nil
}

// Константы режима для access(2). В syscall они не экспортируются
// платформонезависимо.
const (
	unixXOK = 0x1
	unixWOK = 0x2
	unixROK = 0x4
)
