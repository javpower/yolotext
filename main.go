package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	_ "image/gif"
	_ "image/png"
)

// ==================== 1. 基础工具函数 ====================

// SmartCompress 智能压缩
func SmartCompress(img image.Image, outPath string, maxKB int) error {
	quality := 95
	minQuality := 20
	for quality >= minQuality {
		buf := new(bytes.Buffer)
		err := jpeg.Encode(buf, img, &jpeg.Options{Quality: quality})
		if err != nil {
			return err
		}
		if buf.Len()/1024 <= maxKB {
			return os.WriteFile(outPath, buf.Bytes(), 0644)
		}
		quality -= 5
	}
	f, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer f.Close()
	return jpeg.Encode(f, img, &jpeg.Options{Quality: minQuality})
}

// DirectCopy 直接复制
func DirectCopy(src, dst string) error {
	sourceFileStat, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !sourceFileStat.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", src)
	}
	source, err := os.Open(src)
	if err != nil {
		return err
	}
	defer source.Close()
	destination, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer destination.Close()
	_, err = io.Copy(destination, source)
	return err
}

// ConvertJsonToYolo JSON转YOLO
func ConvertJsonToYolo(jsonPath string, imgW, imgH int, classMap map[string]int) ([]string, error) {
	fileBytes, err := os.ReadFile(jsonPath)
	if err != nil {
		return nil, err
	}

	type LabelMeShape struct {
		Label  string      `json:"label"`
		Points [][]float64 `json:"points"`
	}
	type LabelMeJSON struct {
		Shapes []LabelMeShape `json:"shapes"`
		Labels []struct {
			Name string  `json:"name"`
			X1   float64 `json:"x1"`
			Y1   float64 `json:"y1"`
			X2   float64 `json:"x2"`
			Y2   float64 `json:"y2"`
		} `json:"labels"`
	}

	var data LabelMeJSON
	if err := json.Unmarshal(fileBytes, &data); err != nil {
		return nil, err
	}
	var yoloLines []string

	add := func(cls int, x1, y1, x2, y2 float64) {
		w := x2 - x1
		h := y2 - y1
		cx := x1 + w/2.0
		cy := y1 + h/2.0
		line := fmt.Sprintf("%d %.6f %.6f %.6f %.6f", cls, cx/float64(imgW), cy/float64(imgH), w/float64(imgW), h/float64(imgH))
		yoloLines = append(yoloLines, line)
	}

	for _, shape := range data.Shapes {
		if id, ok := classMap[shape.Label]; ok && len(shape.Points) > 0 {
			minX, minY := math.MaxFloat64, math.MaxFloat64
			maxX, maxY := -math.MaxFloat64, -math.MaxFloat64
			for _, p := range shape.Points {
				if len(p) >= 2 {
					if p[0] < minX {
						minX = p[0]
					}
					if p[0] > maxX {
						maxX = p[0]
					}
					if p[1] < minY {
						minY = p[1]
					}
					if p[1] > maxY {
						maxY = p[1]
					}
				}
			}
			add(id, minX, minY, maxX, maxY)
		}
	}
	for _, lbl := range data.Labels {
		if id, ok := classMap[lbl.Name]; ok {
			add(id, lbl.X1, lbl.Y1, lbl.X2, lbl.Y2)
		}
	}
	return yoloLines, nil
}

// ==================== 2. 核心组件：交互式画布 (画框+删除) ====================

type BoxData struct {
	Cls  int
	Rect fyne.Size
	Pos  fyne.Position
	Raw  string // 原始行文本
}

// InteractiveImage 继承 BaseWidget，处理所有鼠标事件
type InteractiveImage struct {
	widget.BaseWidget

	// 数据
	imgObj    *canvas.Image
	boxes     []BoxData
	origW     float32
	origH     float32
	labelPath string

	// 绘图状态
	drawing     bool
	dragStart   fyne.Position
	currentDrag fyne.Position
	tempRect    *canvas.Rectangle // 正在画的框（蓝色）

	// 回调
	onRefreshReq func()
	parentWin    fyne.Window
}

func NewInteractiveImage(win fyne.Window, img image.Image, labelPath string, onRefresh func()) *InteractiveImage {
	ii := &InteractiveImage{
		parentWin:    win,
		labelPath:    labelPath,
		onRefreshReq: onRefresh,
		origW:        float32(img.Bounds().Dx()),
		origH:        float32(img.Bounds().Dy()),
	}
	ii.ExtendBaseWidget(ii)

	ii.imgObj = canvas.NewImageFromImage(img)
	ii.imgObj.FillMode = canvas.ImageFillOriginal
	ii.imgObj.Resize(fyne.NewSize(ii.origW, ii.origH))

	// 初始化绘制用的临时框 (蓝色，初始隐藏)
	ii.tempRect = canvas.NewRectangle(color.RGBA{0, 0, 255, 50})
	ii.tempRect.StrokeWidth = 2
	ii.tempRect.StrokeColor = color.RGBA{0, 0, 255, 255}
	ii.tempRect.Hide()

	return ii
}

// LoadBoxes 加载标注框数据
func (ii *InteractiveImage) LoadBoxes(bs []BoxData) {
	ii.boxes = bs
	ii.Refresh()
}

// CreateRenderer 负责渲染：图片 -> 红色框(已有) -> 蓝色框(正在画)
func (ii *InteractiveImage) CreateRenderer() fyne.WidgetRenderer {
	return &interactiveRenderer{ii: ii}
}

type interactiveRenderer struct {
	ii *InteractiveImage
}

func (r *interactiveRenderer) MinSize() fyne.Size {
	return fyne.NewSize(r.ii.origW, r.ii.origH)
}

func (r *interactiveRenderer) Layout(s fyne.Size) {
	r.ii.imgObj.Resize(s)
	r.ii.imgObj.Move(fyne.NewPos(0, 0))
}

func (r *interactiveRenderer) Refresh() {
	canvas.Refresh(r.ii)
}

func (r *interactiveRenderer) Objects() []fyne.CanvasObject {
	objs := []fyne.CanvasObject{r.ii.imgObj}

	// 1. 渲染已有的框 (红色)
	for _, b := range r.ii.boxes {
		// 框
		rect := canvas.NewRectangle(color.RGBA{255, 0, 0, 40})
		rect.StrokeWidth = 3
		rect.StrokeColor = color.RGBA{255, 0, 0, 255}
		rect.Resize(b.Rect)
		rect.Move(b.Pos)

		// 文字标签
		txt := canvas.NewText(fmt.Sprintf("%d", b.Cls), color.RGBA{255, 255, 0, 255})
		txt.TextStyle.Bold = true
		txt.TextSize = 14
		txt.Move(fyne.NewPos(b.Pos.X, b.Pos.Y-18))

		objs = append(objs, rect, txt)
	}

	// 2. 渲染正在画的框 (蓝色)
	if r.ii.drawing {
		// 计算当前的矩形
		x1 := math.Min(float64(r.ii.dragStart.X), float64(r.ii.currentDrag.X))
		y1 := math.Min(float64(r.ii.dragStart.Y), float64(r.ii.currentDrag.Y))
		w := math.Abs(float64(r.ii.dragStart.X) - float64(r.ii.currentDrag.X))
		h := math.Abs(float64(r.ii.dragStart.Y) - float64(r.ii.currentDrag.Y))

		r.ii.tempRect.Move(fyne.NewPos(float32(x1), float32(y1)))
		r.ii.tempRect.Resize(fyne.NewSize(float32(w), float32(h)))
		r.ii.tempRect.Show()
		objs = append(objs, r.ii.tempRect)
	} else {
		r.ii.tempRect.Hide()
	}

	return objs
}

func (r *interactiveRenderer) Destroy() {}

// --- 事件处理 ---

// Cursor 鼠标样式
func (ii *InteractiveImage) Cursor() desktop.Cursor {
	return desktop.CrosshairCursor
}

// Dragged 拖拽事件 (用于画框)
func (ii *InteractiveImage) Dragged(e *fyne.DragEvent) {
	if !ii.drawing {
		ii.drawing = true
		ii.dragStart = e.Position.Subtract(e.Dragged)
	}
	ii.currentDrag = e.Position
	ii.Refresh()
}

// DragEnd 拖拽结束 (弹出对话框保存)
func (ii *InteractiveImage) DragEnd() {
	if !ii.drawing {
		return
	}
	ii.drawing = false

	x1 := float64(math.Min(float64(ii.dragStart.X), float64(ii.currentDrag.X)))
	y1 := float64(math.Min(float64(ii.dragStart.Y), float64(ii.currentDrag.Y)))
	w := float64(math.Abs(float64(ii.dragStart.X) - float64(ii.currentDrag.X)))
	h := float64(math.Abs(float64(ii.dragStart.Y) - float64(ii.currentDrag.Y)))

	if w < 5 || h < 5 {
		ii.Refresh()
		return
	}

	entry := widget.NewEntry()
	entry.SetPlaceHolder("输入ID")

	dlg := dialog.NewForm("新建标注", "确定", "取消", []*widget.FormItem{
		widget.NewFormItem("类别 ID:", entry),
	}, func(ok bool) {
		if ok {
			clsID, err := strconv.Atoi(entry.Text)
			if err == nil {
				ii.appendLabelToFile(clsID, x1, y1, w, h)
			}
		}
		ii.Refresh()
	}, ii.parentWin)

	dlg.Resize(fyne.NewSize(300, 150))
	dlg.Show()
}

// Tapped 点击事件 (用于删除)
func (ii *InteractiveImage) Tapped(e *fyne.PointEvent) {
	for i := len(ii.boxes) - 1; i >= 0; i-- {
		b := ii.boxes[i]
		if e.Position.X >= b.Pos.X && e.Position.X <= b.Pos.X+b.Rect.Width &&
			e.Position.Y >= b.Pos.Y && e.Position.Y <= b.Pos.Y+b.Rect.Height {

			dialog.ShowConfirm("删除标注", fmt.Sprintf("确认删除类别 %d 的这个框?", b.Cls), func(ok bool) {
				if ok {
					ii.removeLabelFromFile(b.Raw)
				}
			}, ii.parentWin)
			return
		}
	}
}

func (ii *InteractiveImage) appendLabelToFile(cls int, x, y, w, h float64) {
	f, err := os.OpenFile(ii.labelPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	cx := x + w/2.0
	cy := y + h/2.0
	normCx := cx / float64(ii.origW)
	normCy := cy / float64(ii.origH)
	normW := w / float64(ii.origW)
	normH := h / float64(ii.origH)
	line := fmt.Sprintf("\n%d %.6f %.6f %.6f %.6f", cls, normCx, normCy, normW, normH)
	f.WriteString(line)
	ii.onRefreshReq()
}

func (ii *InteractiveImage) removeLabelFromFile(targetRaw string) {
	content, _ := os.ReadFile(ii.labelPath)
	lines := strings.Split(string(content), "\n")
	var newLines []string
	deleted := false
	for _, l := range lines {
		if !deleted && strings.TrimSpace(l) == strings.TrimSpace(targetRaw) {
			deleted = true
			continue
		}
		if strings.TrimSpace(l) != "" {
			newLines = append(newLines, l)
		}
	}
	os.WriteFile(ii.labelPath, []byte(strings.Join(newLines, "\n")), 0644)
	ii.onRefreshReq()
}

// ==================== 3. 预览窗口 ====================

func ShowPreviewWindow(parent fyne.App, datasetDir string) {
	win := parent.NewWindow("数据集审核 (拖拽画框 / 点击红框删除)")
	win.Resize(fyne.NewSize(1200, 800))

	fileListWidget := widget.NewList(nil, nil, nil)

	// 使用 NewWithoutLayout 作为初始内容，防止 Fyne 布局计算空指针崩溃
	scrollContainer := container.NewScroll(container.NewWithoutLayout())

	statusLabel := widget.NewLabel("准备就绪")
	statusLabel.TextStyle.Bold = true

	var currentFiles []string
	var currentSubsets []string
	var currentImgPath, currentLabelPath string

	loadFiles := func() {
		currentFiles = []string{}
		currentSubsets = []string{}
		for _, sub := range []string{"train", "val", "test"} {
			dir := filepath.Join(datasetDir, "images", sub)
			files, _ := os.ReadDir(dir)
			for _, f := range files {
				ext := strings.ToLower(filepath.Ext(f.Name()))
				if ext == ".jpg" || ext == ".png" || ext == ".bmp" || ext == ".jpeg" {
					currentFiles = append(currentFiles, f.Name())
					currentSubsets = append(currentSubsets, sub)
				}
			}
		}
		fileListWidget.Refresh()
	}

	var reloadCurrentItem func()
	reloadCurrentItem = func() {
		if currentImgPath == "" {
			return
		}
		statusLabel.SetText(fmt.Sprintf("加载中: %s", filepath.Base(currentImgPath)))

		go func(imgPath, labelPath string) {
			f, err := os.Open(imgPath)
			if err != nil {
				return
			}
			img, _, err := image.Decode(f)
			f.Close()
			if err != nil {
				return
			}

			origW := float32(img.Bounds().Dx())
			origH := float32(img.Bounds().Dy())

			var boxList []BoxData
			if _, err := os.Stat(labelPath); err == nil {
				content, _ := os.ReadFile(labelPath)
				lines := strings.Split(string(content), "\n")
				for _, line := range lines {
					parts := strings.Fields(line)
					if len(parts) >= 5 {
						cls, _ := strconv.Atoi(parts[0])
						cx, _ := strconv.ParseFloat(parts[1], 64)
						cy, _ := strconv.ParseFloat(parts[2], 64)
						w, _ := strconv.ParseFloat(parts[3], 64)
						h, _ := strconv.ParseFloat(parts[4], 64)

						rectW := float32(w) * origW
						rectH := float32(h) * origH
						x1 := (float32(cx) * origW) - (rectW / 2.0)
						y1 := (float32(cy) * origH) - (rectH / 2.0)

						boxList = append(boxList, BoxData{
							Cls: cls, Rect: fyne.NewSize(rectW, rectH), Pos: fyne.NewPos(x1, y1), Raw: line,
						})
					}
				}
			}

			interactiveWidget := NewInteractiveImage(win, img, labelPath, reloadCurrentItem)
			interactiveWidget.LoadBoxes(boxList)
			interactiveWidget.Resize(fyne.NewSize(origW, origH)) // 必须显式设置

			scrollContainer.Content = interactiveWidget
			scrollContainer.Refresh()

			statusLabel.SetText(fmt.Sprintf("%s [%.0fx%.0f] | 标注: %d | 操作: 拖拽新建, 点击删除",
				filepath.Base(imgPath), origW, origH, len(boxList)))

		}(currentImgPath, currentLabelPath)
	}

	fileListWidget.Length = func() int { return len(currentFiles) }
	fileListWidget.CreateItem = func() fyne.CanvasObject { return widget.NewLabel("f") }
	fileListWidget.UpdateItem = func(i widget.ListItemID, o fyne.CanvasObject) {
		o.(*widget.Label).SetText(fmt.Sprintf("[%s] %s", currentSubsets[i], currentFiles[i]))
	}
	fileListWidget.OnSelected = func(id widget.ListItemID) {
		currentImgPath = filepath.Join(datasetDir, "images", currentSubsets[id], currentFiles[id])
		base := strings.TrimSuffix(currentFiles[id], filepath.Ext(currentFiles[id]))
		currentLabelPath = filepath.Join(datasetDir, "labels", currentSubsets[id], base+".txt")
		reloadCurrentItem()
	}

	loadFiles()

	split := container.NewHSplit(
		container.NewBorder(widget.NewLabelWithStyle("文件列表", fyne.TextAlignCenter, fyne.TextStyle{Bold: true}), nil, nil, nil, fileListWidget),
		container.NewBorder(nil, statusLabel, nil, nil, scrollContainer),
	)
	split.SetOffset(0.2)
	win.SetContent(split)
	win.Show()
}

// ==================== 4. 主程序 (含Windows崩溃修复) ====================

func main() {
	// 使用 NewWithID 解决 Warning
	myApp := app.NewWithID("yolo.tools.fix")
	myWindow := myApp.NewWindow("YOLO 数据集工具 (Windows E盘修复版)")
	myWindow.Resize(fyne.NewSize(1000, 700))

	// 数据源
	listData := []string{}
	listWidget := widget.NewList(
		func() int { return len(listData) },
		func() fyne.CanvasObject { return widget.NewLabel("path") },
		func(i widget.ListItemID, o fyne.CanvasObject) {
			o.(*widget.Label).SetText(listData[i])
			o.(*widget.Label).Truncation = fyne.TextTruncateEllipsis
		},
	)
	btnAdd := widget.NewButtonWithIcon("添加文件夹", theme.FolderNewIcon(), func() {
		dialog.ShowFolderOpen(func(uri fyne.ListableURI, err error) {
			if err == nil && uri != nil {
				listData = append(listData, uri.Path())
				listWidget.Refresh()
			}
		}, myWindow)
	})
	btnClear := widget.NewButtonWithIcon("清空", theme.DeleteIcon(), func() {
		listData = []string{}
		listWidget.Refresh()
	})
	leftPane := container.NewBorder(
		container.NewVBox(widget.NewLabelWithStyle("数据源", fyne.TextAlignCenter, fyne.TextStyle{Bold: true}), container.NewGridWithColumns(2, btnAdd, btnClear)),
		nil, nil, nil, listWidget,
	)

	// 配置
	entryOut := widget.NewEntry()
	entryOut.SetPlaceHolder("选择保存位置...")
	btnOut := widget.NewButton("浏览", func() {
		dialog.ShowFolderOpen(func(uri fyne.ListableURI, err error) {
			if err == nil && uri != nil {
				entryOut.SetText(uri.Path())
			}
		}, myWindow)
	})
	entryClasses := widget.NewEntry()
	entryClasses.SetPlaceHolder("例如: hole, nut")
	entryTrain := widget.NewEntry()
	entryTrain.SetText("0.8")
	entryVal := widget.NewEntry()
	entryVal.SetText("0.2")
	checkEnableProc := widget.NewCheck("启用压缩/转格式", nil)
	checkEnableProc.SetChecked(true)
	entryKB := widget.NewEntry()
	entryKB.SetText("500")

	cardOutput := widget.NewCard("配置", "", container.NewVBox(
		widget.NewLabel("输出目录:"), container.NewBorder(nil, nil, nil, btnOut, entryOut),
		widget.NewLabel("类别:"), entryClasses,
	))
	cardParams := widget.NewCard("选项", "", container.NewVBox(
		widget.NewLabel("比例 (Train/Val):"), container.NewGridWithColumns(2, entryTrain, entryVal),
		checkEnableProc, container.NewBorder(nil, nil, widget.NewLabel("MaxKB:"), nil, entryKB),
	))

	// 运行
	progressBar := widget.NewProgressBar()
	logArea := widget.NewMultiLineEntry()
	logArea.Disable()
	logArea.TextStyle.Monospace = true
	var logLock sync.Mutex
	logFunc := func(msg string) {
		logLock.Lock()
		defer logLock.Unlock()
		logArea.SetText(logArea.Text + msg + "\n")
		logArea.CursorRow = len(strings.Split(logArea.Text, "\n"))
		logArea.Refresh()
	}

	// 【核心修复】btnRun 逻辑
	btnRun := widget.NewButtonWithIcon("开始执行", theme.MediaPlayIcon(), func() {
		// 基础校验
		if len(listData) == 0 {
			dialog.ShowError(fmt.Errorf("错误：未添加数据源"), myWindow)
			return
		}
		if entryOut.Text == "" {
			dialog.ShowError(fmt.Errorf("错误：未选择输出目录"), myWindow)
			return
		}
		if entryClasses.Text == "" {
			dialog.ShowError(fmt.Errorf("错误：未填写类别"), myWindow)
			return
		}

		progressBar.SetValue(0)
		logArea.SetText("初始化中...\n")

		// 获取参数
		outDir := entryOut.Text
		doProc := checkEnableProc.Checked
		maxKB, _ := strconv.Atoi(entryKB.Text)
		trainR, _ := strconv.ParseFloat(entryTrain.Text, 64)
		valR, _ := strconv.ParseFloat(entryVal.Text, 64)
		clsList := strings.Split(entryClasses.Text, ",")
		clsMap := make(map[string]int)
		for i, c := range clsList {
			clsMap[strings.TrimSpace(c)] = i
		}

		go func() {
			// 【Panic 捕获】防止 Windows 静默崩溃
			defer func() {
				if r := recover(); r != nil {
					dialog.ShowError(fmt.Errorf("程序发生异常:\n%v", r), myWindow)
				}
			}()

			logFunc(">>> 开始扫描...")
			type FilePair struct{ ImgPath, JsonPath string }
			var tasks []FilePair

			for _, d := range listData {
				files, err := os.ReadDir(d)
				if err != nil {
					logFunc("读取错误: " + d)
					continue
				}
				for _, f := range files {
					if !f.IsDir() {
						ext := strings.ToLower(filepath.Ext(f.Name()))
						if ext == ".jpg" || ext == ".png" || ext == ".bmp" || ext == ".jpeg" {
							base := strings.TrimSuffix(f.Name(), filepath.Ext(f.Name()))
							tasks = append(tasks, FilePair{filepath.Join(d, f.Name()), filepath.Join(d, base+".json")})
						}
					}
				}
			}

			if len(tasks) == 0 {
				logFunc("!!! 未找到图片")
				dialog.ShowInformation("提示", "未找到图片，请检查路径", myWindow)
				return
			}

			r := rand.New(rand.NewSource(time.Now().UnixNano()))
			r.Shuffle(len(tasks), func(i, j int) { tasks[i], tasks[j] = tasks[j], tasks[i] })

			// 创建目录 (带 Panic 检查)
			for _, s := range []string{"train", "val", "test"} {
				if err := os.MkdirAll(filepath.Join(outDir, "images", s), 0755); err != nil {
					panic("无法创建目录: " + err.Error())
				}
				os.MkdirAll(filepath.Join(outDir, "labels", s), 0755)
			}

			total := len(tasks)
			trainC := int(float64(total) * trainR)
			valC := int(float64(total) * valR)
			var wg sync.WaitGroup
			limit := make(chan struct{}, 4)

			for i, t := range tasks {
				limit <- struct{}{}
				wg.Add(1)
				sub := "test"
				if i < trainC {
					sub = "train"
				} else if i < trainC+valC {
					sub = "val"
				}

				go func(idx int, task FilePair, subset string) {
					defer wg.Done()
					defer func() { <-limit }()

					// 单个任务容错
					defer func() { recover() }()

					base := strings.TrimSuffix(filepath.Base(task.ImgPath), filepath.Ext(task.ImgPath))
					var imgW, imgH int

					if doProc {
						f, err := os.Open(task.ImgPath)
						if err == nil {
							img, _, err := image.Decode(f)
							f.Close()
							if err == nil {
								imgW, imgH = img.Bounds().Dx(), img.Bounds().Dy()
								SmartCompress(img, filepath.Join(outDir, "images", subset, base+".jpg"), maxKB)
							}
						}
					} else {
						f, err := os.Open(task.ImgPath)
						if err == nil {
							cfg, _, err := image.DecodeConfig(f)
							f.Close()
							if err == nil {
								imgW, imgH = cfg.Width, cfg.Height
								DirectCopy(task.ImgPath, filepath.Join(outDir, "images", subset, base+filepath.Ext(task.ImgPath)))
							}
						}
					}

					if _, err := os.Stat(task.JsonPath); err == nil && imgW > 0 {
						lines, err := ConvertJsonToYolo(task.JsonPath, imgW, imgH, clsMap)
						if err == nil {
							os.WriteFile(filepath.Join(outDir, "labels", subset, base+".txt"), []byte(strings.Join(lines, "\n")), 0644)
						}
					}
					progressBar.SetValue(float64(idx+1) / float64(total))
				}(i, t, sub)
			}
			wg.Wait()

			yaml := fmt.Sprintf("path: %s\ntrain: images/train\nval: images/val\ntest: images/test\nnames:\n", outDir)
			invMap := make(map[int]string)
			for k, v := range clsMap {
				invMap[v] = k
			}
			for i := 0; i < len(invMap); i++ {
				yaml += fmt.Sprintf("  %d: %s\n", i, invMap[i])
			}
			os.WriteFile(filepath.Join(outDir, "data.yaml"), []byte(yaml), 0644)

			logFunc(">>> 完成！")
			dialog.ShowInformation("完成", "数据集处理完毕", myWindow)
		}()
	})

	btnPreview := widget.NewButtonWithIcon("打开审核工具", theme.VisibilityIcon(), func() {
		if entryOut.Text == "" {
			dialog.ShowInformation("提示", "请先选择输出目录", myWindow)
			return
		}
		ShowPreviewWindow(myApp, entryOut.Text)
	})

	rightPane := container.NewBorder(
		container.NewPadded(container.NewGridWithColumns(2, cardOutput, cardParams)),
		container.NewPadded(container.NewVBox(progressBar, container.NewHBox(btnRun, layout.NewSpacer(), btnPreview))),
		nil, nil, container.NewPadded(logArea),
	)

	split := container.NewHSplit(leftPane, rightPane)
	split.SetOffset(0.3)
	myWindow.SetContent(split)
	myWindow.ShowAndRun()
}
