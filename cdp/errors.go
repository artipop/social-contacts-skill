package cdp

import (
	"errors"
	"fmt"
)

// The failures a user can act on. Each is returned wrapped with details, so
// check them with errors.Is.
var (
	// ErrNotEnabled: there is no Chrome to connect to. Either Chrome is not
	// running, or remote debugging has not been switched on in it.
	ErrNotEnabled = errors.New("Chrome не запущен или в нём не включена удалённая отладка: " +
		"запустите Chrome 144 или новее, откройте chrome://inspect/#remote-debugging " +
		"и включите удалённую отладку")

	// ErrProfileUnreadable: the Chrome profile directory exists but this
	// process may not read it. On macOS this is the privacy protection of
	// other apps' data, not a file permission.
	ErrProfileUnreadable = errors.New("нет доступа к папке профиля Chrome: " +
		"на macOS разрешите терминалу или агенту доступ к Google Chrome в " +
		"«Системные настройки → Конфиденциальность и безопасность → Файлы и папки»")

	// ErrApprovalPending: Chrome is holding the connection until the user
	// answers its "Allow remote debugging?" prompt, and they have not yet.
	ErrApprovalPending = errors.New("Chrome ждёт разрешения на подключение: " +
		"нажмите «Разрешить» в окне Chrome и повторите")

	// ErrDenied: Chrome refused the connection, which with auto-connect means
	// the user declined the prompt.
	ErrDenied = errors.New("Chrome отклонил подключение: в окне Chrome с вопросом " +
		"об удалённой отладке нужно нажать «Разрешить»")

	// ErrTabClosed: the tab being driven was closed, or Chrome detached from
	// it.
	ErrTabClosed = errors.New("вкладка закрыта")

	// ErrDisconnected: the connection to Chrome was lost — Chrome quit,
	// debugging was switched off, or the socket failed.
	ErrDisconnected = errors.New("соединение с Chrome потеряно")
)

// ProtocolError is an error answer to a CDP command that does not map to one of
// the errors above.
type ProtocolError struct {
	Method  string
	Code    int
	Message string
}

func (e *ProtocolError) Error() string {
	return fmt.Sprintf("CDP %s: %s (%d)", e.Method, e.Message, e.Code)
}
