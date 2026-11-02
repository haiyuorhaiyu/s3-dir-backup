//go:build windows

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

const (
	wmCommand  = 0x0111
	wmDestroy  = 0x0002
	wmClose    = 0x0010
	wmApp      = 0x8000
	wmGUIEvent = wmApp + 1
	bnClicked  = 0

	wsOverlappedWindow = 0x00CF0000
	wsChild            = 0x40000000
	wsVisible          = 0x10000000
	wsBorder           = 0x00800000
	wsTabStop          = 0x00010000
	wsVScroll          = 0x00200000
	esLeft             = 0x0000
	esAutoHScroll      = 0x0080
	esMultiline        = 0x0004
	esWantReturn       = 0x1000
	esPassword         = 0x0020
	esReadOnly         = 0x0800
	lbsNotify          = 0x0001
	bsPushButton       = 0x00000000
	bsAutoCheckbox     = 0x00000003
	bmGetCheck         = 0x00F0
	bmSetCheck         = 0x00F1
	lbAddString        = 0x0180
	lbDeleteString     = 0x0182
	lbGetCount         = 0x018B
	lbGetTextLen       = 0x018A
	lbGetText          = 0x0189
	lbGetCurSel        = 0x0188
	colorWindow        = 5
	idEndpoint         = 101
	idRegion           = 102
	idBucket           = 103
	idPrefix           = 104
	idAccessKey        = 105
	idSecretKey        = 106
	idSessionToken     = 107
	idPathStyle        = 108
	idSourcePath       = 109
	idSources          = 110
	idBrowse           = 111
	idAddSource        = 112
	idRemoveSource     = 113
	idSave             = 114
	idCheck            = 115
	idBackup           = 116
	idDaily            = 117
	idStop             = 118
	idLog              = 119
	idCancel           = 120
)

var (
	user32            = syscall.NewLazyDLL("user32.dll")
	kernel32          = syscall.NewLazyDLL("kernel32.dll")
	shell32           = syscall.NewLazyDLL("shell32.dll")
	ole32             = syscall.NewLazyDLL("ole32.dll")
	procRegisterClass = user32.NewProc("RegisterClassExW")
	procCreateWindow  = user32.NewProc("CreateWindowExW")
	procDefWindow     = user32.NewProc("DefWindowProcW")
	procShowWindow    = user32.NewProc("ShowWindow")
	procUpdateWindow  = user32.NewProc("UpdateWindow")
	procGetMessage    = user32.NewProc("GetMessageW")
	procTranslate     = user32.NewProc("TranslateMessage")
	procDispatch      = user32.NewProc("DispatchMessageW")
	procPostQuit      = user32.NewProc("PostQuitMessage")
	procPostMessage   = user32.NewProc("PostMessageW")
	procSendMessage   = user32.NewProc("SendMessageW")
	procGetText       = user32.NewProc("GetWindowTextW")
	procGetTextLength = user32.NewProc("GetWindowTextLengthW")
	procSetText       = user32.NewProc("SetWindowTextW")
	procMessageBox    = user32.NewProc("MessageBoxW")
	procEnableWindow  = user32.NewProc("EnableWindow")
	procGetModule     = kernel32.NewProc("GetModuleHandleW")
	procBrowseFolder  = shell32.NewProc("SHBrowseForFolderW")
	procGetFolderPath = shell32.NewProc("SHGetPathFromIDListW")
	procFree          = ole32.NewProc("CoTaskMemFree")
	guiInstance       *guiState
	guiEvents         = make(chan guiEvent, 32)
)

type wndClassEx struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     uintptr
	hIcon         uintptr
	hCursor       uintptr
	hbrBackground uintptr
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       uintptr
}

type browseInfo struct {
	hwndOwner      uintptr
	pidlRoot       uintptr
	pszDisplayName *uint16
	lpszTitle      *uint16
	ulFlags        uint32
	lpfnCallback   uintptr
	lParam         uintptr
	iImage         int32
}

type msg struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	ptX     int32
	ptY     int32
}

type guiEvent struct {
	text string
	done bool
}

type guiState struct {
	hwnd       uintptr
	endpoint   uintptr
	region     uintptr
	bucket     uintptr
	prefix     uintptr
	accessKey  uintptr
	secretKey  uintptr
	sessionTok uintptr
	pathStyle  uintptr
	sourcePath uintptr
	sources    uintptr
	log        uintptr
	configPath string
	sourceDirs []string
	busy       bool
	dailyStop  context.CancelFunc
	operation  context.CancelFunc
}

func runGUI() {
	runtime.LockOSThread()
	instance, _, _ := procGetModule.Call(0)
	className := utf16("S3DirectoryBackupWindow")
	wndProc := syscall.NewCallback(windowProc)
	wc := wndClassEx{cbSize: uint32(unsafe.Sizeof(wndClassEx{})), lpfnWndProc: wndProc, hInstance: instance, hbrBackground: colorWindow + 1, lpszClassName: className}
	if result, _, err := procRegisterClass.Call(uintptr(unsafe.Pointer(&wc))); result == 0 && err != syscall.Errno(0) {
		messageBox(0, "注册窗口失败: "+err.Error(), "S3 目录备份")
		return
	}
	hwnd := createWindow(className, "S3 目录备份", wsOverlappedWindow, 100, 80, 980, 760, 0, 0, instance)
	if hwnd == 0 {
		messageBox(0, "创建窗口失败", "S3 目录备份")
		return
	}
	exePath, _ := os.Executable()
	guiInstance = &guiState{hwnd: hwnd, configPath: filepath.Join(filepath.Dir(exePath), "backup.json")}
	buildGUI(guiInstance)
	procShowWindow.Call(hwnd, 1)
	procUpdateWindow.Call(hwnd)
	loadGUIConfig(guiInstance)
	var message msg
	for {
		result, _, _ := procGetMessage.Call(uintptr(unsafe.Pointer(&message)), 0, 0, 0)
		if int32(result) <= 0 {
			break
		}
		procTranslate.Call(uintptr(unsafe.Pointer(&message)))
		procDispatch.Call(uintptr(unsafe.Pointer(&message)))
	}
}

func windowProc(hwnd uintptr, message uint32, wParam, lParam uintptr) uintptr {
	if guiInstance == nil {
		return procDefWindowResult(hwnd, message, wParam, lParam)
	}
	switch message {
	case wmCommand:
		if uint16(wParam&0xffff) != 0 && uint16((wParam>>16)&0xffff) == bnClicked {
			handleGUICommand(uint16(wParam & 0xffff))
		}
	case wmGUIEvent:
		drainGUIEvents()
	case wmClose:
		stopDaily(guiInstance)
		if guiInstance.operation != nil {
			guiInstance.operation()
		}
		user32.NewProc("DestroyWindow").Call(hwnd)
	case wmDestroy:
		procPostQuit.Call(0)
	default:
		return procDefWindowResult(hwnd, message, wParam, lParam)
	}
	return 0
}

func procDefWindowResult(hwnd uintptr, message uint32, wParam, lParam uintptr) uintptr {
	result, _, _ := procDefWindow.Call(hwnd, uintptr(message), wParam, lParam)
	return result
}

func buildGUI(app *guiState) {
	createLabel(app.hwnd, "Endpoint", 20, 18, 80, 20)
	app.endpoint = createEdit(app.hwnd, idEndpoint, 20, 40, 430, 26, "https://s3.example.com", false)
	createLabel(app.hwnd, "Region", 490, 18, 80, 20)
	app.region = createEdit(app.hwnd, idRegion, 490, 40, 200, 26, "us-east-1", false)
	createLabel(app.hwnd, "Bucket", 710, 18, 80, 20)
	app.bucket = createEdit(app.hwnd, idBucket, 710, 40, 230, 26, "your-bucket", false)
	createLabel(app.hwnd, "远端前缀", 20, 78, 80, 20)
	app.prefix = createEdit(app.hwnd, idPrefix, 20, 100, 430, 26, "daily-backup", false)
	app.pathStyle = createButton(app.hwnd, idPathStyle, "Path-style 寻址", 490, 98, 180, 28, bsAutoCheckbox)
	procSendMessage.Call(app.pathStyle, bmSetCheck, 1, 0)
	procEnableWindow.Call(app.pathStyle, 0)

	createLabel(app.hwnd, "Access Key", 20, 138, 110, 20)
	app.accessKey = createEdit(app.hwnd, idAccessKey, 20, 160, 430, 26, "", false)
	createLabel(app.hwnd, "Secret Key", 490, 138, 110, 20)
	app.secretKey = createEdit(app.hwnd, idSecretKey, 490, 160, 450, 26, "", true)
	createLabel(app.hwnd, "SessionToken（可选）", 20, 198, 150, 20)
	app.sessionTok = createEdit(app.hwnd, idSessionToken, 20, 220, 920, 26, "", true)

	createLabel(app.hwnd, "备份目录", 20, 258, 100, 20)
	app.sourcePath = createEdit(app.hwnd, idSourcePath, 20, 280, 670, 26, "", false)
	createButton(app.hwnd, idBrowse, "选择目录", 705, 278, 110, 30, bsPushButton)
	createButton(app.hwnd, idAddSource, "添加目录", 830, 278, 110, 30, bsPushButton)
	app.sources = createListBox(app.hwnd, idSources, 20, 320, 920, 105)
	createButton(app.hwnd, idRemoveSource, "移除选中目录", 20, 435, 130, 30, bsPushButton)

	createButton(app.hwnd, idSave, "保存配置", 20, 485, 120, 34, bsPushButton)
	createButton(app.hwnd, idCheck, "测试连接", 155, 485, 120, 34, bsPushButton)
	createButton(app.hwnd, idBackup, "立即备份", 290, 485, 120, 34, bsPushButton)
	createButton(app.hwnd, idDaily, "启动每日计划", 425, 485, 140, 34, bsPushButton)
	createButton(app.hwnd, idStop, "停止计划", 580, 485, 120, 34, bsPushButton)
	createButton(app.hwnd, idCancel, "取消当前操作", 715, 485, 140, 34, bsPushButton)

	createLabel(app.hwnd, "运行日志", 20, 535, 100, 20)
	app.log = createMultilineEdit(app.hwnd, idLog, 20, 558, 920, 145)
	appendLog("已打开。请填写凭据并添加需要备份的目录。")
}

func handleGUICommand(id uint16) {
	app := guiInstance
	switch id {
	case idBrowse:
		if path := chooseFolder(app.hwnd); path != "" {
			setText(app.sourcePath, path)
		}
	case idAddSource:
		app.addSource()
	case idRemoveSource:
		app.removeSource()
	case idSave:
		app.save()
	case idCheck:
		app.check()
	case idBackup:
		app.backup()
	case idDaily:
		app.startDaily()
	case idStop:
		stopDaily(app)
		appendLog("已停止每日计划。")
	case idCancel:
		cancelOperation(app)
	}
}

func (app *guiState) addSource() {
	path := strings.TrimSpace(getText(app.sourcePath))
	if path == "" {
		messageBox(app.hwnd, "请选择或填写目录。", "添加目录")
		return
	}
	for _, existing := range app.sourceDirs {
		if filepath.Clean(existing) == filepath.Clean(path) {
			return
		}
	}
	app.sourceDirs = append(app.sourceDirs, path)
	procSendMessage.Call(app.sources, lbAddString, 0, uintptr(unsafe.Pointer(utf16(path))))
	setText(app.sourcePath, "")
}

func (app *guiState) removeSource() {
	index, _, _ := procSendMessage.Call(app.sources, lbGetCurSel, 0, 0)
	if int32(index) < 0 || int(index) >= len(app.sourceDirs) {
		return
	}
	procSendMessage.Call(app.sources, lbDeleteString, index, 0)
	app.sourceDirs = append(app.sourceDirs[:index], app.sourceDirs[index+1:]...)
}

func (app *guiState) config() Config {
	pathStyle := procSendMessageResult(app.pathStyle, bmGetCheck, 0, 0) != 0
	return Config{Endpoint: getText(app.endpoint), Region: getText(app.region), Bucket: getText(app.bucket), Prefix: getText(app.prefix), AccessKey: getText(app.accessKey), SecretKey: getText(app.secretKey), SessionToken: getText(app.sessionTok), PathStyle: &pathStyle, Sources: app.sourcesConfig()}
}

func (app *guiState) sourcesConfig() []Source {
	result := make([]Source, 0, len(app.sourceDirs))
	names := make(map[string]int)
	for _, path := range app.sourceDirs {
		name := filepath.Base(filepath.Clean(path))
		if name == "." || name == string(filepath.Separator) || name == "" {
			name = "source"
		}
		names[name]++
		if names[name] > 1 {
			name = fmt.Sprintf("%s-%d", name, names[name])
		}
		result = append(result, Source{Name: name, Path: path})
	}
	return result
}

func (app *guiState) save() {
	cfg := app.config()
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err == nil {
		err = os.WriteFile(app.configPath, data, 0600)
	}
	if err != nil {
		messageBox(app.hwnd, "保存失败: "+err.Error(), "保存配置")
		return
	}
	appendLog("配置已保存：" + app.configPath)
}

func (app *guiState) check() {
	if app.busy || app.dailyStop != nil {
		return
	}
	app.busy = true
	appendLog("正在测试 S3 连接...")
	cfg := app.config()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	app.operation = cancel
	go func() {
		defer cancel()
		err := checkS5cmdStorage(ctx, cfg, sendGUIEvent)
		if err != nil {
			sendGUIDone("连接失败: " + err.Error())
		} else {
			sendGUIDone("连接成功，Bucket 可访问（备份前缀为空也是正常的）。")
		}
	}()
}

func (app *guiState) backup() {
	if app.busy || app.dailyStop != nil {
		return
	}
	app.busy = true
	appendLog("开始备份...")
	cfg := app.config()
	ctx, cancel := context.WithCancel(context.Background())
	app.operation = cancel
	go func() {
		defer cancel()
		result, err := syncWithS5cmd(ctx, cfg, sendGUIEvent)
		if err != nil {
			sendGUIDone("备份失败: " + err.Error())
		} else {
			sendGUIDone(fmt.Sprintf("备份完成：%d 个目录已与存储同步。", result.Source))
		}
	}()
}

func (app *guiState) startDaily() {
	if app.busy {
		appendLog("当前操作尚未结束，暂不能启动每日计划。")
		return
	}
	if app.dailyStop != nil {
		appendLog("每日计划已经在运行。")
		return
	}
	app.busy = true
	ctx, cancel := context.WithCancel(context.Background())
	app.dailyStop = cancel
	app.operation = cancel
	appendLog("每日计划已启动：立即执行一次，之后每天零点执行。")
	cfg := app.config()
	go func() {
		for {
			result, err := syncWithS5cmd(ctx, cfg, sendGUIEvent)
			if err != nil {
				sendGUIDone("计划备份失败: " + err.Error())
			} else {
				sendGUIDone(fmt.Sprintf("计划备份完成：%d 个目录已同步。", result.Source))
			}
			now := time.Now()
			tomorrow := now.AddDate(0, 0, 1)
			next := time.Date(tomorrow.Year(), tomorrow.Month(), tomorrow.Day(), 0, 0, 0, 0, tomorrow.Location())
			timer := time.NewTimer(time.Until(next))
			select {
			case <-ctx.Done():
				timer.Stop()
				sendGUIEvent("每日计划已停止。")
				return
			case <-timer.C:
			}
		}
	}()
}

func loadGUIConfig(app *guiState) {
	cfg, err := loadConfig(app.configPath)
	if err != nil {
		if !os.IsNotExist(err) {
			appendLog("配置加载失败：" + err.Error())
		}
		return
	}
	setText(app.endpoint, cfg.Endpoint)
	setText(app.region, cfg.Region)
	setText(app.bucket, cfg.Bucket)
	setText(app.prefix, cfg.Prefix)
	setText(app.accessKey, cfg.AccessKey)
	setText(app.secretKey, cfg.SecretKey)
	setText(app.sessionTok, cfg.SessionToken)
	if cfg.PathStyle == nil || *cfg.PathStyle {
		procSendMessage.Call(app.pathStyle, bmSetCheck, 1, 0)
	} else {
		procSendMessage.Call(app.pathStyle, bmSetCheck, 0, 0)
	}
	for _, source := range cfg.Sources {
		app.sourceDirs = append(app.sourceDirs, source.Path)
		procSendMessage.Call(app.sources, lbAddString, 0, uintptr(unsafe.Pointer(utf16(source.Path))))
	}
	appendLog("已加载配置：" + app.configPath)
}

func stopDaily(app *guiState) {
	if app != nil && app.dailyStop != nil {
		app.dailyStop()
		app.dailyStop = nil
		app.operation = nil
		app.busy = false
	}
}

func cancelOperation(app *guiState) {
	if app != nil && app.dailyStop != nil {
		stopDaily(app)
		appendLog("已请求停止每日计划。")
		return
	}
	if app != nil && app.operation != nil {
		app.operation()
		appendLog("已发送取消请求；正在等待当前网络操作结束。")
	}
}

func sendGUIEvent(text string) {
	sendGUIEventWithStatus(guiEvent{text: text})
}

func sendGUIDone(text string) {
	sendGUIEventWithStatus(guiEvent{text: text, done: true})
}

func sendGUIEventWithStatus(event guiEvent) {
	if event.done {
		guiEvents <- event
	} else {
		select {
		case guiEvents <- event:
		default:
		}
	}
	if guiInstance != nil {
		procPostMessage.Call(guiInstance.hwnd, wmGUIEvent, 0, 0)
	}
}
func drainGUIEvents() {
	for {
		select {
		case event := <-guiEvents:
			appendLog(event.text)
			if guiInstance != nil && event.done {
				guiInstance.busy = false
				if guiInstance.dailyStop == nil {
					guiInstance.operation = nil
				}
			}
		default:
			return
		}
	}
}
func appendLog(text string) {
	if guiInstance == nil || guiInstance.log == 0 {
		return
	}
	const maxLogBytes = 24000
	line := time.Now().Format("15:04:05 ") + text + "\r\n"
	old := getText(guiInstance.log)
	updated := old + line
	if len(updated) > maxLogBytes {
		updated = updated[len(updated)-maxLogBytes:]
		if newline := strings.Index(updated, "\n"); newline >= 0 {
			updated = updated[newline+1:]
		}
	}
	setText(guiInstance.log, updated)
}

func createLabel(parent uintptr, text string, x, y, width, height int) uintptr {
	return createControl("STATIC", text, wsChild|wsVisible, x, y, width, height, parent, 0)
}
func createEdit(parent uintptr, id uint16, x, y, width, height int, text string, password bool) uintptr {
	style := uint32(wsChild | wsVisible | wsBorder | wsTabStop | esAutoHScroll | esLeft)
	if password {
		style |= esPassword
	}
	return createControl("EDIT", text, style, x, y, width, height, parent, uintptr(id))
}

func createMultilineEdit(parent uintptr, id uint16, x, y, width, height int) uintptr {
	style := uint32(wsChild | wsVisible | wsBorder | wsVScroll | esMultiline | esWantReturn | esAutoHScroll | esLeft | esReadOnly)
	return createControl("EDIT", "", style, x, y, width, height, parent, uintptr(id))
}
func createButton(parent uintptr, id uint16, text string, x, y, width, height int, buttonStyle uint32) uintptr {
	return createControl("BUTTON", text, wsChild|wsVisible|wsTabStop|buttonStyle, x, y, width, height, parent, uintptr(id))
}
func createListBox(parent uintptr, id uint16, x, y, width, height int) uintptr {
	return createControl("LISTBOX", "", wsChild|wsVisible|wsBorder|wsVScroll|lbsNotify, x, y, width, height, parent, uintptr(id))
}
func createControl(className, text string, style uint32, x, y, width, height int, parent, id uintptr) uintptr {
	instance, _, _ := procGetModule.Call(0)
	hwnd, _, _ := procCreateWindow.Call(0, uintptr(unsafe.Pointer(utf16(className))), uintptr(unsafe.Pointer(utf16(text))), uintptr(style), uintptr(x), uintptr(y), uintptr(width), uintptr(height), parent, id, instance, 0)
	return hwnd
}
func getText(hwnd uintptr) string {
	length, _, _ := procGetTextLength.Call(hwnd)
	buffer := make([]uint16, int(length)+1)
	procGetText.Call(hwnd, uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)))
	return syscall.UTF16ToString(buffer)
}
func setText(hwnd uintptr, value string) {
	procSetText.Call(hwnd, uintptr(unsafe.Pointer(utf16(value))))
}
func procSendMessageResult(hwnd uintptr, message uint32, wParam, lParam uintptr) uintptr {
	result, _, _ := procSendMessage.Call(hwnd, uintptr(message), wParam, lParam)
	return result
}
func messageBox(hwnd uintptr, text, title string) {
	procMessageBox.Call(hwnd, uintptr(unsafe.Pointer(utf16(text))), uintptr(unsafe.Pointer(utf16(title))), 0x00000010)
}
func createWindow(className *uint16, title string, style uint32, x, y, width, height int, parent, id, instance uintptr) uintptr {
	hwnd, _, _ := procCreateWindow.Call(0, uintptr(unsafe.Pointer(className)), uintptr(unsafe.Pointer(utf16(title))), uintptr(style), uintptr(x), uintptr(y), uintptr(width), uintptr(height), parent, id, instance, 0)
	return hwnd
}
func utf16(value string) *uint16 { result, _ := syscall.UTF16PtrFromString(value); return result }

func chooseFolder(owner uintptr) string {
	buffer := make([]uint16, 260)
	title := utf16("选择需要备份的目录")
	info := browseInfo{hwndOwner: owner, pszDisplayName: &buffer[0], lpszTitle: title, ulFlags: 0x00000001 | 0x00000040}
	pidl, _, _ := procBrowseFolder.Call(uintptr(unsafe.Pointer(&info)))
	if pidl == 0 {
		return ""
	}
	defer procFree.Call(pidl)
	pathBuffer := make([]uint16, 32768)
	ok, _, _ := procGetFolderPath.Call(pidl, uintptr(unsafe.Pointer(&pathBuffer[0])))
	if ok == 0 {
		return ""
	}
	return syscall.UTF16ToString(pathBuffer)
}
