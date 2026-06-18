package main

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/schollz/progressbar/v3"
	"golang.org/x/image/draw"
)

type Config struct {
	ChatID        int64
	BotAPI        string
	TgAPIURL      string
	PhotoExts     []string
	VideoExts     []string
	AllowSpilFile bool
	AllowMaxSize  int64
	SpilMaxSize   float64
}

type VideoMetaCache struct {
	Width      int  `json:"width"`
	Height     int  `json:"height"`
	Duration   int  `json:"duration"`
	IsPortrait bool `json:"is_portrait"`
}

type OrgVideoCache struct {
	OrigFilename   string `json:"orig_filename"`
	Size           int64  `json:"size"`
	Width          int    `json:"width"`
	Height         int    `json:"height"`
	Duration       int    `json:"duration"`
	LocalCachePath string `json:"local_cache_path"`
	Uploaded       bool   `json:"uploaded"`
}

type FilePayload struct {
	FieldKey      string
	FilePath      string
	ShowProgress  bool
	NeedTranscode bool
}

type TgMediaItem struct {
	Type              string `json:"type"`
	Media             string `json:"media"`
	Caption           string `json:"caption,omitempty"`
	Width             int    `json:"width,omitempty"`
	Height            int    `json:"height,omitempty"`
	Duration          int    `json:"duration,omitempty"`
	SupportsStreaming bool   `json:"supports_streaming,omitempty"`
	Thumb             string `json:"thumb,omitempty"`
}

type fileSortItem struct {
	path    string
	modTime time.Time
	creTime time.Time
	size    int64
}

func main() {
	var flagArgs []string
	var positionalArgs []string
	for i := 1; i < len(os.Args); i++ {
		arg := os.Args[i]
		if strings.HasPrefix(arg, "-") {
			flagArgs = append(flagArgs, arg)
			if !strings.Contains(arg, "=") {
				flagName := strings.TrimLeft(arg, "-")
				if flagName == "title" || flagName == "t" || flagName == "test" || flagName == "type" || flagName == "n" || flagName == "s" || flagName == "sort" || flagName == "spil" {
					if i+1 < len(os.Args) {
						flagArgs = append(flagArgs, os.Args[i+1])
						i++
					}
				}
			}
		} else {
			positionalArgs = append(positionalArgs, arg)
		}
	}
	os.Args = append(append([]string{os.Args[0]}, flagArgs...), positionalArgs...)

	var titleFlag, testFlag, typeFilterFlag, sortFlag string
	var cacheForceFlag, transcodeFlag, spilFlag bool
	var batchSizeFlag, sleepDurationFlag int

	flag.StringVar(&titleFlag, "title", "", "Global title")
	flag.StringVar(&testFlag, "t", "", "Test mode")
	flag.StringVar(&testFlag, "test", "", "Test mode long")
	flag.BoolVar(&cacheForceFlag, "cache-force", false, "Force metadata")
	flag.BoolVar(&cacheForceFlag, "cf", false, "Force metadata short")
	flag.IntVar(&batchSizeFlag, "n", 10, "Batch size")
	flag.StringVar(&typeFilterFlag, "type", "all", "Type filter")
	flag.IntVar(&sleepDurationFlag, "s", 4, "Sleep duration")
	flag.BoolVar(&transcodeFlag, "transcode", false, "Enable transcode")
	flag.StringVar(&sortFlag, "sort", "name", "Sort strategy")
	flag.BoolVar(&spilFlag, "spil", false, "Force split")
	flag.Parse()

	tailArgs := flag.Args()
	if len(tailArgs) == 0 {
		fmt.Println("Error: Please specify a path.")
		os.Exit(1)
	}
	targetPath := tailArgs[0]

	finalTestMode := testFlag
	if finalTestMode == "" {
		finalTestMode = flag.Lookup("test").Value.String()
	}
	if batchSizeFlag > 10 || batchSizeFlag <= 0 {
		batchSizeFlag = 10
	}
	if sleepDurationFlag < 0 {
		sleepDurationFlag = 0
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		os.Exit(1)
	}
	baseDir := filepath.Join(homeDir, ".tgup")
	cacheDir := filepath.Join(baseDir, "cache")
	logDir := filepath.Join(baseDir, "logs")
	_ = os.MkdirAll(cacheDir, 0755)
	_ = os.MkdirAll(logDir, 0755)

	config, err := loadConfig(filepath.Join(baseDir, "config.conf"))
	if err != nil {
		os.Exit(1)
	}
	if spilFlag {
		config.AllowSpilFile = true
	}

	rawFiles, err := collectFiles(targetPath, sortFlag)
	if err != nil {
		os.Exit(1)
	}

	var files []string
	typeFilterFlag = strings.ToLower(strings.TrimSpace(typeFilterFlag))
	for _, file := range rawFiles {
		ext := strings.ToLower(filepath.Ext(file))
		isPhoto := contains(config.PhotoExts, ext)
		isVideo := contains(config.VideoExts, ext)

		if typeFilterFlag != "all" && typeFilterFlag != "pic" && typeFilterFlag != "video" && typeFilterFlag != "vedio" {
			targetExt := typeFilterFlag
			if !strings.HasPrefix(targetExt, ".") {
				targetExt = "." + targetExt
			}
			if ext != targetExt {
				continue
			}
		} else {
			if typeFilterFlag == "pic" && !isPhoto {
				continue
			}
			if (typeFilterFlag == "video" || typeFilterFlag == "vedio") && !isVideo {
				continue
			}
		}
		if !isPhoto && !isVideo {
			continue
		}
		files = append(files, file)
	}

	if len(files) == 0 {
		fmt.Println("No valid files found.")
		return
	}

	if finalTestMode == "list" {
		totalBatches := (len(files) + batchSizeFlag - 1) / batchSizeFlag
		fmt.Printf("\n📋 预检就绪：过滤与排序完成的文件列表 (共 %d 个，每批 %d 个，将分 %d 批循环执行):\n", len(files), batchSizeFlag, totalBatches)
		fmt.Println(strings.Repeat("-", 60))
		for idx, file := range files {
			fi, err := os.Stat(file)
			sizeStr, cTimeStr, mTimeStr := "Unknown", "--", "--"
			if err == nil {
				sizeStr = formatSize(fi.Size())
				mTime := fi.ModTime()
				cTime := mTime
				if sys := fi.Sys(); sys != nil {
					v := reflect.ValueOf(sys).Elem()
					statField := v.FieldByName("Ctim")
					if !statField.IsValid() {
						statField = v.FieldByName("Ctimespec")
					}
					if statField.IsValid() {
						secField := statField.FieldByName("Sec")
						nsecField := statField.FieldByName("Nsec")
						if secField.IsValid() && nsecField.IsValid() {
							cTime = time.Unix(secField.Int(), nsecField.Int())
						}
					}
				}
				cTimeStr = cTime.Format("2006-01-02 15:04")
				mTimeStr = mTime.Format("2006-01-02 15:04")
			}
			fmt.Printf("[%03d] %s (%s | C:%s M:%s)\n", idx+1, filepath.Base(file), sizeStr, cTimeStr, mTimeStr)
			if (idx+1)%batchSizeFlag == 0 || (idx+1) == len(files) {
				currentBatch := (idx / batchSizeFlag) + 1
				itemsInBatch := batchSizeFlag
				if (idx+1) == len(files) && len(files)%batchSizeFlag != 0 {
					itemsInBatch = len(files) % batchSizeFlag
				}
				fmt.Printf("📦 👆 以上为 [第 %d / %d 批次] 预检包 (包含 %d 个文件) 👆\n", currentBatch, totalBatches, itemsInBatch)
				fmt.Println(strings.Repeat("-", 60))
			}
		}
		return
	}

	globalCaption := strings.TrimSpace(titleFlag)
	if globalCaption == "" && finalTestMode != "curl" {
		fmt.Print("Do you want to add a caption for this upload? (Press Enter to skip): ")
		input, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		globalCaption = strings.TrimSpace(input)
	}

	var bot *tgbotapi.BotAPI
	if finalTestMode != "curl" {
		bot, err = tgbotapi.NewBotAPIWithAPIEndpoint(config.BotAPI, config.TgAPIURL+"/bot%s/%s")
		if err != nil {
			os.Exit(1)
		}
	}

	var currentBatch []string
	for _, file := range files {
		ext := strings.ToLower(filepath.Ext(file))
		fi, err := os.Stat(file)
		isLargeVideo := err == nil && contains(config.VideoExts, ext) && fi.Size() > 2000*1024*1024

		if isLargeVideo {
			if !config.AllowSpilFile {
				fmt.Printf("⚠️ 跳过大视频文件 (未激活切片开关 ALLOW_SPIL_FILE): %s (%s)\n", filepath.Base(file), formatSize(fi.Size()))
				continue
			}
			if fi.Size() > config.AllowMaxSize {
				fmt.Printf("❌ 超过最大文件尺寸限制 (ALLOW_MAX_SIZE: %s)，强制跳过: %s (%s)\n", formatSize(config.AllowMaxSize), filepath.Base(file), formatSize(fi.Size()))
				continue
			}
			if len(currentBatch) > 0 {
				preProcessAndUpload(bot, config, currentBatch, cacheDir, cacheForceFlag, globalCaption, logDir, transcodeFlag, finalTestMode)
				currentBatch = nil
				if sleepDurationFlag > 0 {
					time.Sleep(time.Duration(sleepDurationFlag) * time.Second)
				}
			}
			handleSplitVideoUpload(bot, config, file, cacheDir, cacheForceFlag, globalCaption, logDir, transcodeFlag, finalTestMode)
			if sleepDurationFlag > 0 {
				time.Sleep(time.Duration(sleepDurationFlag) * time.Second)
			}
		} else {
			currentBatch = append(currentBatch, file)
			if len(currentBatch) == batchSizeFlag {
				preProcessAndUpload(bot, config, currentBatch, cacheDir, cacheForceFlag, globalCaption, logDir, transcodeFlag, finalTestMode)
				currentBatch = nil
				if sleepDurationFlag > 0 {
					time.Sleep(time.Duration(sleepDurationFlag) * time.Second)
				}
			}
		}
	}
	if len(currentBatch) > 0 {
		preProcessAndUpload(bot, config, currentBatch, cacheDir, cacheForceFlag, globalCaption, logDir, transcodeFlag, finalTestMode)
	}
	if finalTestMode != "curl" {
		fmt.Println("\nAll uploads completed successfully!")
	}
}

func preProcessAndUpload(bot *tgbotapi.BotAPI, config *Config, batch []string, cacheDir string, cacheForceFlag bool, globalCaption string, logDir string, transcodeFlag bool, finalTestMode string) {
	if finalTestMode != "curl" {
		for _, f := range batch {
			if contains(config.VideoExts, strings.ToLower(filepath.Ext(f))) {
				_, _, _, _, _, _ = getVideoDataUnified(f, cacheDir, cacheForceFlag)
				time.Sleep(600 * time.Millisecond)
			}
		}
	}
	if finalTestMode == "curl" {
		generateCurlCommand(config, batch, globalCaption, cacheDir, cacheForceFlag)
	} else {
		uploadMediaGroup(bot, config.ChatID, batch, cacheDir, globalCaption, config, logDir, cacheForceFlag, transcodeFlag)
	}
}

func handleSplitVideoUpload(bot *tgbotapi.BotAPI, cfg *Config, origPath string, cacheDir string, cacheForce bool, globalCaption string, logDir string, transcodeFlag bool, finalTestMode string) {
	ext := strings.ToLower(filepath.Ext(origPath))
	baseName := filepath.Base(origPath)
	fi, err := os.Stat(origPath)
	if err != nil {
		return
	}

	hasherToken := sha1.New()
	hasherToken.Write([]byte(origPath + strconv.FormatInt(fi.Size(), 10) + strconv.FormatInt(fi.ModTime().Unix(), 10)))
	token := hex.EncodeToString(hasherToken.Sum(nil))
	jsonPath := filepath.Join(cacheDir, fmt.Sprintf("org_%s.json", token))
	localOrgPath := filepath.Join(cacheDir, fmt.Sprintf("org_%s%s", token, ext))

	var cacheData OrgVideoCache
	hitCacheFile := false

	if !cacheForce {
		if jsonBytes, err := os.ReadFile(jsonPath); err == nil {
			if json.Unmarshal(jsonBytes, &cacheData) == nil {
				if cacheData.Uploaded {
					fmt.Printf("🎉 [元数据强缓存命中] 该超大视频已成功投递，跳过！\n文件：%s\n", baseName)
					return
				}
				if fiLocal, errFile := os.Stat(cacheData.LocalCachePath); errFile == nil && fiLocal.Size() == fi.Size() {
					fmt.Printf("📦 [原始文件缓存命中] 本地发现完整视频缓冲，直接切片！\n路径：%s\n", cacheData.LocalCachePath)
					localOrgPath = cacheData.LocalCachePath
					hitCacheFile = true
				}
			}
		}
	}

	if !hitCacheFile {
		fmt.Println("⏳ 本地未见缓存，正在从网盘高速流式拉取数据到本地 Cache...")
		src, err := os.Open(origPath)
		if err != nil {
			return
		}
		defer src.Close()

		tmpFile, err := os.CreateTemp(cacheDir, "tgup_org_download_")
		if err != nil {
			return
		}
		defer os.Remove(tmpFile.Name())

		bar := progressbar.NewOptions64(fi.Size(), progressbar.OptionSetDescription("[本地快速缓存中]"), progressbar.OptionSetWriter(os.Stderr), progressbar.OptionShowBytes(true), progressbar.OptionSetWidth(15), progressbar.OptionThrottle(65), progressbar.OptionShowCount(), progressbar.OptionOnCompletion(func() { fmt.Fprint(os.Stderr, "\n") }), progressbar.OptionSpinnerType(14), progressbar.OptionFullWidth())
		_, err = io.Copy(io.MultiWriter(tmpFile, bar), src)
		tmpFile.Close()
		if err != nil {
			return
		}

		_ = os.Remove(localOrgPath)
		if err := os.Rename(tmpFile.Name(), localOrgPath); err != nil {
			return
		}

		w, h, duration := probeLocalVideo(localOrgPath)
		cacheData = OrgVideoCache{OrigFilename: baseName, Size: fi.Size(), Width: w, Height: h, Duration: duration, LocalCachePath: localOrgPath, Uploaded: false}
		metaBytes, _ := json.MarshalIndent(cacheData, "", "  ")
		_ = os.WriteFile(jsonPath, metaBytes, 0644)
	}

	totalSizeGB := float64(cacheData.Size) / (1024.0 * 1024.0 * 1024.0)
	segmentTimeSec := int(math.Floor((cfg.SpilMaxSize / totalSizeGB) * float64(cacheData.Duration)))
	if segmentTimeSec <= 0 {
		segmentTimeSec = 300
	}

	outputPattern := filepath.Join(cacheDir, fmt.Sprintf("split_%s_%%03d%s", token, ext))
	fmt.Printf("⚙️ 正在调用 FFmpeg 启动秒级无损流拷贝切割 (单片时间: %d 秒)...\n", segmentTimeSec)
	cmd := exec.Command("ffmpeg", "-y", "-i", localOrgPath, "-c", "copy", "-map", "0", "-f", "segment", "-segment_time", strconv.Itoa(segmentTimeSec), "-reset_timestamps", "1", outputPattern)

	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return
	}

	splitPieces, err := filepath.Glob(filepath.Join(cacheDir, fmt.Sprintf("split_%s_*%s", token, ext)))
	if err != nil || len(splitPieces) == 0 {
		return
	}

	sort.Slice(splitPieces, func(i, j int) bool { return isLessNatural(splitPieces[i], splitPieces[j]) })
	fmt.Printf("📦 切片大功告成！裂变生成 %d 个分段小视频，开始作为独立相册投递...\n", len(splitPieces))
	preProcessAndUpload(bot, cfg, splitPieces, cacheDir, cacheForce, globalCaption, logDir, transcodeFlag, finalTestMode)

	fmt.Println("🧹 上传结束，正在清刷本地大体积视频缓存大文件...")
	_ = os.Remove(localOrgPath)
	for _, piece := range splitPieces {
		_ = os.Remove(piece)
		hasherName := sha1.New()
		hasherName.Write([]byte(piece))
		pieceToken := hex.EncodeToString(hasherName.Sum(nil))
		_ = os.Remove(filepath.Join(cacheDir, pieceToken+".jpg"))
	}
	cacheData.Uploaded = true
	metaBytes, _ := json.MarshalIndent(cacheData, "", "  ")
	_ = os.WriteFile(jsonPath, metaBytes, 0644)
	fmt.Println("✨ 本地大视频缓存已全数粉碎，元数据成功留存。")
}

func probeLocalVideo(path string) (w, h, duration int) {
	cmd := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries", "stream=width,height,duration", "-of", "default=noprint_wrappers=1", path)
	out, err := cmd.Output()
	if err != nil {
		return 0, 0, 0
	}
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.Split(line, "=")
		if len(parts) != 2 {
			continue
		}
		val := parts[1]
		switch parts[0] {
		case "width":
			w, _ = strconv.Atoi(val)
		case "height":
			h, _ = strconv.Atoi(val)
		case "duration":
			if idx := strings.Index(val, "."); idx != -1 {
				val = val[:idx]
			}
			duration, _ = strconv.Atoi(val)
		}
	}
	return
}

func checkAndResizePhoto(photoPath string, cacheDir string, cacheForce bool) (string, error) {
	file, err := os.Open(photoPath)
	if err != nil {
		return photoPath, err
	}
	defer file.Close()
	imgConfig, _, err := image.DecodeConfig(file)
	if err != nil {
		return photoPath, err
	}
	totalPixels := int64(imgConfig.Width) * int64(imgConfig.Height)
	if totalPixels <= 19500000 {
		return photoPath, nil
	}

	ratio := math.Sqrt(19500000.0 / float64(totalPixels))
	newWidth, newHeight := int(float64(imgConfig.Width)*ratio), int(float64(imgConfig.Height)*ratio)
	_, _ = file.Seek(0, 0)
	srcImg, _, err := image.Decode(file)
	if err != nil {
		return photoPath, err
	}

	dstImg := image.NewRGBA(image.Rect(0, 0, newWidth, newHeight))
	draw.CatmullRom.Scale(dstImg, dstImg.Bounds(), srcImg, srcImg.Bounds(), draw.Over, nil)
	hasher := sha1.New()
	hasher.Write([]byte(fmt.Sprintf("%s_%d_%d", photoPath, newWidth, newHeight)))
	tmpPath := filepath.Join(cacheDir, fmt.Sprintf("resized_20m_%s.jpg", hex.EncodeToString(hasher.Sum(nil))))

	if !cacheForce {
		if _, err := os.Stat(tmpPath); err == nil {
			return tmpPath, nil
		}
	} else {
		_ = os.Remove(tmpPath)
	}

	outFile, err := os.Create(tmpPath)
	if err != nil {
		return photoPath, err
	}
	defer outFile.Close()
	_ = jpeg.Encode(outFile, dstImg, &jpeg.Options{Quality: 95})
	return tmpPath, nil
}

func getImageDimensions(path string) (int, int) {
	file, err := os.Open(path)
	if err != nil {
		return 0, 0
	}
	defer file.Close()
	cfg, _, err := image.DecodeConfig(file)
	if err != nil {
		return 0, 0
	}
	return cfg.Width, cfg.Height
}

func generateCurlCommand(cfg *Config, files []string, globalCaption string, cacheDir string, cacheForce bool) {
	var mediaJSONArray []TgMediaItem
	var fileFormFields []string
	isFirstItem := true
	photoIdx, videoIdx, docIdx := 1, 1, 1

	for _, file := range files {
		ext := strings.ToLower(filepath.Ext(file))
		isPhoto, isVideo := contains(cfg.PhotoExts, ext), contains(cfg.VideoExts, ext)
		currentCaption := ""
		if isFirstItem && globalCaption != "" {
			currentCaption = globalCaption
			isFirstItem = false
		}

		var attachKey string
		if isVideo {
			attachKey = fmt.Sprintf("video%d", videoIdx)
			width, height, duration, thumbPath, isPortraitVideo, thumbErr := getVideoDataUnified(file, cacheDir, cacheForce)
			var thumbValue string
			if thumbErr == nil && thumbPath != "" {
				thumbFormKey := fmt.Sprintf("thumb_video%d", videoIdx)
				thumbValue = "attach://" + thumbFormKey
				fileFormFields = append(fileFormFields, fmt.Sprintf("     -F \"%s=@%s\"", thumbFormKey, thumbPath))
			}
			if isPortraitVideo && width > height {
				width, height = height, width
			}
			mediaJSONArray = append(mediaJSONArray, TgMediaItem{Type: "video", Media: "attach://" + attachKey, Caption: currentCaption, Width: width, Height: height, Duration: duration, SupportsStreaming: true, Thumb: thumbValue})
			videoIdx++
		} else if isPhoto {
			attachKey = fmt.Sprintf("photo%d", photoIdx)
			finalPhoto, _ := checkAndResizePhoto(file, cacheDir, cacheForce)
			mediaJSONArray = append(mediaJSONArray, TgMediaItem{Type: "photo", Media: "attach://" + attachKey, Caption: currentCaption})
			fileFormFields = append(fileFormFields, fmt.Sprintf("     -F \"%s=@%s\"", attachKey, finalPhoto))
			photoIdx++
		} else {
			attachKey = fmt.Sprintf("doc%d", docIdx)
			mediaJSONArray = append(mediaJSONArray, TgMediaItem{Type: "document", Media: "attach://" + attachKey, Caption: currentCaption})
			fileFormFields = append(fileFormFields, fmt.Sprintf("     -F \"%s=@%s\"", attachKey, file))
			docIdx++
		}
		if isVideo {
			fileFormFields = append(fileFormFields, fmt.Sprintf("     -F \"%s=@%s\"", attachKey, file))
		}
	}
	if len(mediaJSONArray) == 0 {
		return
	}
	mediaBytes, _ := json.MarshalIndent(mediaJSONArray, "     ", "  ")
	fmt.Printf("curl -X POST \"%s/bot%s/sendMediaGroup\" \\\n     -F \"chat_id=%d\" \\\n     -F \"media=%s\" \\\n%s\n", cfg.TgAPIURL, cfg.BotAPI, cfg.ChatID, strings.ReplaceAll(string(mediaBytes), `"`, `\"`), strings.Join(fileFormFields, " \\\n"))
}

func writeLog(logDir string, filename string, content string) {
	f, err := os.OpenFile(filepath.Join(logDir, filename), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(content)
}

func loadConfig(path string) (*Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	cfg := &Config{PhotoExts: []string{".jpg", ".jpeg", ".png"}, VideoExts: []string{".mp4", ".mkv", ".avi", ".mov", ".m4v"}, AllowSpilFile: false, AllowMaxSize: 10 * 1024 * 1024 * 1024, SpilMaxSize: 1.5}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key, val := strings.ToUpper(strings.TrimSpace(parts[0])), strings.TrimSpace(parts[1])

		switch key {
		case "CHAT_ID":
			id, _ := strconv.ParseInt(val, 10, 64)
			cfg.ChatID = id
		case "BOT_API":
			cfg.BotAPI = val
		case "TG_API_URL":
			cfg.TgAPIURL = val
		case "ALLOW_SPIL_FILE":
			cfg.AllowSpilFile = strings.ToLower(val) == "true"
		case "ALLOW_MAX_SIZE":
			vL := strings.ToLower(val)
			if strings.HasSuffix(vL, "g") {
				num, _ := strconv.ParseInt(strings.TrimSuffix(vL, "g"), 10, 64)
				cfg.AllowMaxSize = num * 1024 * 1024 * 1024
			} else if strings.HasSuffix(vL, "m") {
				num, _ := strconv.ParseInt(strings.TrimSuffix(vL, "m"), 10, 64)
				cfg.AllowMaxSize = num * 1024 * 1024
			} else {
				num, _ := strconv.ParseInt(val, 10, 64)
				cfg.AllowMaxSize = num
			}
		case "SPIL_MAX_SIZE":
			f, _ := strconv.ParseFloat(val, 64)
			cfg.SpilMaxSize = f
		case "PHOTO_EXTS", "VIDEO_EXTS":
			exts := strings.Split(strings.ToLower(val), ",")
			var clean []string
			for _, e := range exts {
				e = strings.TrimSpace(e)
				if e != "" {
					if !strings.HasPrefix(e, ".") {
						e = "." + e
					}
					clean = append(clean, e)
				}
			}
			if len(clean) > 0 {
				if key == "PHOTO_EXTS" {
					cfg.PhotoExts = clean
				} else {
					cfg.VideoExts = clean
				}
			}
		}
	}
	return cfg, scanner.Err()
}

func collectFiles(targetPath string, sortType string) ([]string, error) {
	fi, err := os.Stat(targetPath)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return []string{targetPath}, nil
	}

	var items []fileSortItem
	err = filepath.Walk(targetPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			if strings.HasPrefix(info.Name(), ".") {
				return nil
			}
			mTime := info.ModTime()
			cTime := mTime
			if sys := info.Sys(); sys != nil {
				v := reflect.ValueOf(sys).Elem()
				statField := v.FieldByName("Ctim")
				if !statField.IsValid() {
					statField = v.FieldByName("Ctimespec")
				}
				if statField.IsValid() {
					secField := statField.FieldByName("Sec")
					nsecField := statField.FieldByName("Nsec")
					if secField.IsValid() && nsecField.IsValid() {
						cTime = time.Unix(secField.Int(), nsecField.Int())
					}
				}
			}
			items = append(items, fileSortItem{path: path, modTime: mTime, creTime: cTime, size: info.Size()})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	sortType = strings.ToLower(strings.TrimSpace(sortType))
	switch sortType {
	case "mod", "mtime":
		sort.Slice(items, func(i, j int) bool { return items[i].modTime.Before(items[j].modTime) })
	case "create", "ctime":
		sort.Slice(items, func(i, j int) bool { return items[i].creTime.Before(items[j].creTime) })
	case "size", "size_asc", "sizeasc":
		sort.Slice(items, func(i, j int) bool { return items[i].size < items[j].size })
	case "size_desc", "sizedesc":
		sort.Slice(items, func(i, j int) bool { return items[i].size > items[j].size })
	default:
		sort.Slice(items, func(i, j int) bool { return isLessNatural(items[i].path, items[j].path) })
	}

	var sortedFiles []string
	for _, item := range items {
		sortedFiles = append(sortedFiles, item.path)
	}
	return sortedFiles, nil
}

func uploadMediaGroup(bot *tgbotapi.BotAPI, chatID int64, files []string, cacheDir string, globalCaption string, cfg *Config, logDir string, cacheForce bool, transcodeFlag bool) {
	var mediaJSONArray []TgMediaItem
	var payloads []FilePayload
	isFirstItem := true
	photoIdx, videoIdx, docIdx := 1, 1, 1

	for _, file := range files {
		ext := strings.ToLower(filepath.Ext(file))
		isPhoto, isVideo := contains(cfg.PhotoExts, ext), contains(cfg.VideoExts, ext)
		currentCaption := ""
		if isFirstItem && globalCaption != "" {
			currentCaption = globalCaption
			isFirstItem = false
		}

		if isVideo {
			attachKey := fmt.Sprintf("video%d", videoIdx)
			width, height, duration, thumbPath, isPortraitVideo, _ := getVideoDataUnified(file, cacheDir, cacheForce)
			var thumbValue string
			if thumbPath != "" {
				thumbFormKey := fmt.Sprintf("thumb_video%d", videoIdx)
				thumbValue = "attach://" + thumbFormKey
				payloads = append(payloads, FilePayload{FieldKey: thumbFormKey, FilePath: thumbPath, ShowProgress: false, NeedTranscode: false})
			}
			if isPortraitVideo && width > height {
				width, height = height, width
			}
			mediaJSONArray = append(mediaJSONArray, TgMediaItem{Type: "video", Media: "attach://" + attachKey, Caption: currentCaption, Width: width, Height: height, Duration: duration, SupportsStreaming: true, Thumb: thumbValue})
			payloads = append(payloads, FilePayload{FieldKey: attachKey, FilePath: file, ShowProgress: true, NeedTranscode: transcodeFlag})
			videoIdx++
		} else if isPhoto {
			attachKey := fmt.Sprintf("photo%d", photoIdx)
			finalPhotoPath, _ := checkAndResizePhoto(file, cacheDir, cacheForce)
			mediaJSONArray = append(mediaJSONArray, TgMediaItem{Type: "photo", Media: "attach://" + attachKey, Caption: currentCaption})
			payloads = append(payloads, FilePayload{FieldKey: attachKey, FilePath: finalPhotoPath, ShowProgress: true, NeedTranscode: false})
			photoIdx++
		} else {
			attachKey := fmt.Sprintf("doc%d", docIdx)
			mediaJSONArray = append(mediaJSONArray, TgMediaItem{Type: "document", Media: "attach://" + attachKey, Caption: currentCaption})
			payloads = append(payloads, FilePayload{FieldKey: attachKey, FilePath: file, ShowProgress: true, NeedTranscode: false})
			docIdx++
		}
	}

	maxRetries := 5
	for retry := 0; retry < maxRetries; retry++ {
		pipeReader, pipeWriter := io.Pipe()
		multipartWriter := multipart.NewWriter(pipeWriter)
		contentType := multipartWriter.FormDataContentType()

		go func() {
			defer pipeWriter.Close()
			defer multipartWriter.Close()
			_ = multipartWriter.WriteField("chat_id", strconv.FormatInt(chatID, 10))
			mediaJSONBytes, _ := json.Marshal(mediaJSONArray)
			_ = multipartWriter.WriteField("media", string(mediaJSONBytes))

			for _, p := range payloads {
				uploadFileName := filepath.Base(p.FilePath)
				if p.NeedTranscode {
					uploadFileName = strings.TrimSuffix(uploadFileName, filepath.Ext(uploadFileName)) + ".mp4"
				}
				part, err := multipartWriter.CreateFormFile(p.FieldKey, uploadFileName)
				if err != nil {
					return
				}

				if p.NeedTranscode {
					cmd := exec.Command("ffmpeg", "-y", "-threads", "2", "-i", p.FilePath, "-c:v", "libx264", "-preset", "veryfast", "-vf", "scale='if(gt(iw,ih),-2,720)':'if(gt(iw,ih),720,-2)'", "-pix_fmt", "yuv420p", "-profile:v", "main", "-level:v", "4.0", "-c:a", "aac", "-b:a", "128k", "-f", "mp4", "-movflags", "frag_keyframe+empty_moov+default_base_moof", "pipe:1")
					cmd.Stdout = part
					var bar *progressbar.ProgressBar
					if p.ShowProgress {
						bar = progressbar.NewOptions(-1, progressbar.OptionSetDescription(fmt.Sprintf("[⚙️转码上传] %s", filepath.Base(p.FilePath))), progressbar.OptionSetWriter(os.Stderr), progressbar.OptionShowBytes(true), progressbar.OptionSetWidth(15), progressbar.OptionThrottle(65), progressbar.OptionSpinnerType(14), progressbar.OptionFullWidth())
						cmd.Stdout = io.MultiWriter(part, bar)
					}
					_ = cmd.Run()
					if bar != nil {
						fmt.Fprint(os.Stderr, "\n")
					}
				} else {
					file, err := os.Open(p.FilePath)
					if err != nil {
						return
					}
					if p.ShowProgress {
						fi, _ := file.Stat()
						descStr := filepath.Base(p.FilePath)
						if retry > 0 {
							descStr = fmt.Sprintf("[重试-%d] %s", retry, descStr)
						}
						bar := progressbar.NewOptions64(fi.Size(), progressbar.OptionSetDescription(fmt.Sprintf("[Uploading] %s", descStr)), progressbar.OptionSetWriter(os.Stderr), progressbar.OptionShowBytes(true), progressbar.OptionSetWidth(15), progressbar.OptionThrottle(65), progressbar.OptionShowCount(), progressbar.OptionOnCompletion(func() { fmt.Fprint(os.Stderr, "\n") }), progressbar.OptionSpinnerType(14), progressbar.OptionFullWidth())
						proxyReader := progressbar.NewReader(file, bar)
						_, _ = io.Copy(part, &proxyReader)
					} else {
						_, _ = io.Copy(part, file)
					}
					file.Close()
				}
			}
		}()

		req, err := http.NewRequest("POST", fmt.Sprintf("%s/bot%s/sendMediaGroup", cfg.TgAPIURL, cfg.BotAPI), pipeReader)
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", contentType)

		resp, err := (&http.Client{Timeout: 0}).Do(req)
		currentTimeStr := time.Now().Format("2006-01-02 15:04:05")
		fileListStr := strings.Join(files, ", ")

		if err != nil {
			writeLog(logDir, "error.log", fmt.Sprintf("[%s] ERROR: %v | Files: [%s]\n", currentTimeStr, err, fileListStr))
			return
		}
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			fmt.Println("Batch upload success.")
			writeLog(logDir, "ok.log", fmt.Sprintf("[%s] SUCCESS: Status: %d | Files: [%s]\n", currentTimeStr, resp.StatusCode, fileListStr))
			return
		}

		bodyStr := string(respBody)
		if strings.Contains(bodyStr, "Too Many Requests") || strings.Contains(bodyStr, "retry after") {
			waitSeconds := 10
			if idx := strings.Index(bodyStr, "retry after "); idx != -1 {
				sub := bodyStr[idx+len("retry after "):]
				endIdx := 0
				for endIdx < len(sub) && sub[endIdx] >= '0' && sub[endIdx] <= '9' {
					endIdx++
				}
				if endIdx > 0 {
					if s, err := strconv.Atoi(sub[:endIdx]); err == nil {
						waitSeconds = s
					}
				}
			}
			actualWait := waitSeconds + 5 + (retry * 15)
			fmt.Printf("\n⚠️ 触发 Telegram 官方频控。服务器返回: %s\n💤 为彻底清空云端权重，当前第 %d 次重试将主动高精度休眠 %d 秒...\n\n", bodyStr, retry+1, actualWait)
			writeLog(logDir, "error.log", fmt.Sprintf("[%s] RETRY-%d: FloodWait %d sec | Files: [%s]\n", currentTimeStr, retry+1, actualWait, fileListStr))
			time.Sleep(time.Duration(actualWait) * time.Second)
			continue
		}
		fmt.Printf("\nTelegram API Error: Status %d, Body: %s\n", resp.StatusCode, bodyStr)
		writeLog(logDir, "error.log", fmt.Sprintf("[%s] ERROR: Status %d | Body: %s | Files: [%s]\n", currentTimeStr, resp.StatusCode, bodyStr, fileListStr))
		return
	}
}

func getVideoDataUnified(videoPath string, cacheDir string, cacheForce bool) (w, h, duration int, thumbPath string, isPortrait bool, err error) {
	hasherName := sha1.New()
	hasherName.Write([]byte(videoPath))
	token := hex.EncodeToString(hasherName.Sum(nil))
	finalThumbPath := filepath.Join(cacheDir, token+".jpg")
	finalMetaPath := filepath.Join(cacheDir, token+".json")

	isCacheValid := false
	var cachedMeta VideoMetaCache
	if !cacheForce {
		_, errThumb := os.Stat(finalThumbPath)
		metaBytes, errMeta := os.ReadFile(finalMetaPath)
		if errThumb == nil && errMeta == nil {
			if json.Unmarshal(metaBytes, &cachedMeta) == nil {
				tW, tH := getImageDimensions(finalThumbPath)
				if tW > 0 && tH > 0 && tW <= 320 && tH <= 320 {
					isCacheValid = true
				}
			}
		}
	}
	if isCacheValid {
		return cachedMeta.Width, cachedMeta.Height, cachedMeta.Duration, finalThumbPath, cachedMeta.IsPortrait, nil
	}

	_ = os.Remove(finalThumbPath)
	_ = os.Remove(finalMetaPath)
	time.Sleep(200 * time.Millisecond)

	cmdProbe := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries", "stream=width,height,duration", "-of", "default=noprint_wrappers=1", videoPath)
	outProbe, err := cmdProbe.Output()
	if err != nil {
		return 0, 0, 0, "", false, fmt.Errorf("ffprobe cloud failed: %v", err)
	}
	parentScanner := bufio.NewScanner(strings.NewReader(string(outProbe)))
	for parentScanner.Scan() {
		line := parentScanner.Text()
		parts := strings.Split(line, "=")
		if len(parts) != 2 {
			continue
		}
		val := parts[1]
		switch parts[0] {
		case "width":
			w, _ = strconv.Atoi(val)
		case "height":
			h, _ = strconv.Atoi(val)
		case "duration":
			if idx := strings.Index(val, "."); idx != -1 {
				val = val[:idx]
			}
			duration, _ = strconv.Atoi(val)
		}
	}

	time.Sleep(400 * time.Millisecond)
	rawTempJpg := filepath.Join(cacheDir, "raw_extracted_frame.jpg")
	_ = os.Remove(rawTempJpg)
	cmdImg := exec.Command("ffmpeg", "-y", "-i", videoPath, "-ss", "00:00:01", "-vframes", "1", "-q:v", "2", rawTempJpg)
	if err := cmdImg.Run(); err != nil {
		cmdFallback := exec.Command("ffmpeg", "-y", "-i", videoPath, "-vframes", "1", "-q:v", "2", rawTempJpg)
		_ = cmdFallback.Run()
	}
	defer os.Remove(rawTempJpg)

	rawFile, err := os.Open(rawTempJpg)
	if err != nil {
		return w, h, duration, "", false, nil
	}
	srcImg, _, err := image.Decode(rawFile)
	rawFile.Close()
	if err != nil {
		return w, h, duration, "", false, nil
	}

	bounds := srcImg.Bounds()
	srcW, srcH := bounds.Dx(), bounds.Dy()
	maxSize := 320
	var newW, newHeight int
	if srcW > srcH {
		newW = maxSize
		newHeight = int(float64(srcH) * (float64(maxSize) / float64(srcW)))
	} else {
		newHeight = maxSize
		newW = int(float64(srcW) * (float64(maxSize) / float64(srcH)))
	}
	newW = (newW / 2) * 2
	newHeight = (newHeight / 2) * 2

	dstImg := image.NewRGBA(image.Rect(0, 0, newW, newHeight))
	draw.CatmullRom.Scale(dstImg, dstImg.Bounds(), srcImg, srcImg.Bounds(), draw.Over, nil)
	outFile, err := os.Create(finalThumbPath)
	if err == nil {
		_ = jpeg.Encode(outFile, dstImg, &jpeg.Options{Quality: 92})
		outFile.Close()
	}

	if newHeight > newW {
		isPortrait = true
	}
	newCacheData := VideoMetaCache{Width: w, Height: h, Duration: duration, IsPortrait: isPortrait}
	metaBytes, _ := json.Marshal(newCacheData)
	_ = os.WriteFile(finalMetaPath, metaBytes, 0644)

	return w, h, duration, finalThumbPath, isPortrait, nil
}

// 🚀 【完美补齐区】：最尾部核心工具方法函数集
func contains(slice []string, val string) bool {
	for _, item := range slice {
		if strings.EqualFold(item, val) {
			return true
		}
	}
	return false
}

func isLessNatural(a, b string) bool {
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if isDigit(a[i]) && isDigit(b[j]) {
			startA := i
			for i < len(a) && isDigit(a[i]) {
				i++
			}
			numA, _ := strconv.ParseUint(a[startA:i], 10, 64)
			startB := j
			for j < len(b) && isDigit(b[j]) {
				j++
			}
			numB, _ := strconv.ParseUint(b[startB:j], 10, 64)
			if numA != numB {
				return numA < numB
			}
		} else {
			if a[i] != b[j] {
				return a[i] < b[j]
			}
			i++
			j++
		}
	}
	return len(a) < len(b)
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

func formatSize(bytes int64) string {
	if bytes < 1024 {
		return fmt.Sprintf("%d B", bytes)
	}
	if bytes < 1024*1024 {
		return fmt.Sprintf("%.2f KB", float64(bytes)/1024.0)
	}
	if bytes < 1024*1024*1024 {
		return fmt.Sprintf("%.2f MB", float64(bytes)/1024.0/1024.0)
	}
	return fmt.Sprintf("%.2f GB", float64(bytes)/1024.0/1024.0/1024.0)
}
