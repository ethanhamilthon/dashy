# TODO
- [x] bash tool: убрать параметр max_output из схемы инструмента (агент сейчас сам выбирает лимит вывода на каждый вызов). Вынести лимит вывода в settings.json и /settings, агент не может его менять сам. Дефолт как сейчас (8192). (`internal/agent/agent.go`)

## Аудит (лишний код / оверинженеринг)

### Возможные баги
- [x] notify.go: стейл-сокеты в /tmp-фолбэке SocketPath не чистятся removeDeadSockets -> Listen может падать 'address in use'. Убрать removeDeadSockets и dial-проверку, делать os.Remove(sockPath) перед net.Listen (сессия уже под flock)
- [ ] session.go changeSession: чтение a.queue.wake без лока (потенциальная гонка). Переиспользовать a.queue между сессиями, общий конструктор newAgent с New — пропущено: гонки нет: OnPush вызывается один раз в control.New, смена сессии идёт под локом контроллера; переиспользование очереди изменило бы семантику (старые ожидающие сообщения перешли бы в новую сессию)
- [x] agents.go blockOutputs/blockWriter: stderr съедает общий бюджет 64KB и обрезает stdout. Раздельные буферы: capped stdout + stderr tail
- [x] control/controller.go AttachmentsDir читал c.a без лока, пока отложенное переключение сессии (`*a = *next` в changeSession, из горутины хода) переписывает Agent -> гонка данных при вставке из буфера (ctrl+v) во время /new|/resume. Теперь через c.view; тест TestAttachmentsDirDuringSessionSwitch (-race)

### Дублирование / лишнее (agent)
- [x] agent.go:256 и recovery.go:35: дубль сборки function_call_output и парсинга call -> хелперы callOutput / parseCall
- [x] storage.go DefaultTitle: использовать firstUserLine(a.History) вместо Records()
- [x] settings.go: двойная validateSettings в update; лишняя нормализация Providers==nil в LoadSettings
- [x] agent.go/compact.go: лишние refreshSettings (compact.go:149) и Save после steered input; хелпер для повторяющегося 'could not save history'
- [x] notify.go imageSlots: однострочные прокси-методы на Agent, commitImages/sentImages лезут в мьютекс снаружи -> методы на *imageSlots
- [x] timeline.go: Record и Event почти дубли -> Event встраивает Record; объединить case delta/refusal_delta (только объединён case delta/refusal_delta; Event не встраивает Record — сломались бы все литералы Event{Kind:…} и поле Provisional/Preview)
- [ ] retry.go: парсинг Retry-After почти ничего не даёт (клэмп 8s) + строки 'connection reset/refused' — проверить и упростить — пропущено: Retry-After реально удлиняет паузу при 429 (1–2с -> до 8с), а строковые проверки нужны для ошибок стрима без %w-обёртки
- [ ] settings.go: миграция show_lines short/medium — удалить, если старые значения не релизились — пропущено: релизов нет, но прежний дефолт был "short" и мог попасть в локальный settings.json владельца — без миграции он перестанет загружаться

### TUI / control
- [x] control/ops.go OnOff — мёртвый код, удалить
- [x] control/ops.go SettingOptions/step/SettingName: три свитча -> одна таблица строк настроек
- [x] controller.go: инлайн sessionsLocked, убрать nil-guard для emit
- [ ] control/types.go Event.Operation: пишется, но не читается — проверить и удалить — пропущено: поле читается в osc7501 (compact/turn)
- [x] control/queue.go Queue[T]: дженерик ради одного типа -> конкретный тип, возможно перенести в tui
- [x] tui/clipboard.go: обёртки readClipboard/readClipboardWith/clipboardRunner ради одного теста -> упростить
- [x] tui/commands.go: две параллельные таблицы команд (control.Commands + commandIcons) склеиваются по строке -> упростить

### CLI / shell / osc7501
- [x] cli/print_test.go mockTerminalReader не используется; isTerminal: убрать интерфейсную ветку и Cygwin-проверку
- [x] cli/run.go shortResult: лишний guard `s == ""`
- [x] shell.Execute всегда возвращал nil-ошибку -> возвращает Result (runTool и тесты обновлены; перепроверено после переноса лимита вывода в настройки)
- [x] tui/clipboard_test.go csiTestMessage и control/controller_test.go поле summary не используются
- [x] control: observe(op) и deliverLocked() bool — неиспользуемые параметр/результат
- [x] osc7501 Encode: ветка MaxSequence недостижима -> Encode возвращает []byte (граница проверяется тестом на худший случай)
- [x] osc7501 clean: цикл усечения O(n) на огромном заголовке (~430 мс на 1M рун) -> ограниченный цикл; тест
- [x] osc7501: x/term -> go-isatty (одна зависимость на определение TTY; go.mod: term стал indirect)
- [x] cli/print_test.go setupTestHome дублирует setup; execCommand/ручная сборка дублирует build(t)
- [x] ExitError: убрано лишнее `code != 130` в main.go — пропущена остальная часть: перепроектирование типа даёт ~11 строк, но трогает путь выхода и тесты
- [ ] print_test.go: 4 теста со своим JSON response.completed вместо streamOf — пропущено: тесты перестали бы проверять «голую» форму ответа, выигрыш мал
- [ ] shell/images.go CaptureImage/CaptureImageCopy -> одна функция — пропущено: два имени яснее на местах вызова, экономия 3 строки
- [ ] osc7501/tty_other_test.go шим и ci.yml `go build -o /dev/null` — пропущено: безвредно, косметика
- [x] control/ops.go OnOff — уже сделано в первом проходе

Полные отчёты: /Users/erdana/.dashy/subagents/20261105T020730-95733/out (internal/agent), /Users/erdana/.dashy/subagents/20261105T020730-95747/out (tui/control), /Users/erdana/.dashy/subagents/20261105T020739-95888/out (cli/shell/osc7501/tests)
