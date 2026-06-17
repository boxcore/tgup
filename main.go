package main

import (
	"bufio"
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
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/schollz/progressbar/v3"
	"golang.org/x/image/draw"
)

// Config 存储配置文件信息
type Config struct {
	ChatID    int64
	BotAPI    string
	TgAPIURL  string
	PhotoExts []string
	VideoExts []string
}

// VideoMetaCache 用于本地强缓存 WebDAV 视频的元数据，避免重复 ffprobe
type VideoMetaCache struct {
	Width      int  `json:"width"`
	Height     int  `json:"height"`
	Duration   int  `json:"duration"`
	IsPortrait bool `json:"is_portrait"`
}

func main() {
	var titleFlag string
	var testFlag string
	var cacheForceFlag bool

	flag.StringVar(&titleFlag, "title", "", "Specify a global caption title for the media group")
	flag.StringVar(&testFlag, "t", "", "Test mode: use '-t=curl' to print curl command")
	flag.StringVar(&testFlag, "test", "", "Test mode: use '--test=curl' to print curl command")
	flag.BoolVar(&cacheForceFlag, "cache-force", false, "Force regenerate and overwrite existing thumbnails/photos")
	flag.BoolVar(&cacheForceFlag, "cf", false, "Force regenerate and overwrite existing thumbnails/photos (shorthand)")

	flag.Parse()

	tailArgs := flag.Args()
	if len(tailArgs) == 0 {
		fmt.Println("Error: Please specify a file or directory path.")
		fmt.Println("Usage: go run main.go [-t=curl] [--cache-force] [--title='title'] <path>")
		os.Exit(1)
	}
	targetPath := tailArgs[0]

	finalTestMode := testFlag
	if finalTestMode == "" {
		finalTestMode = flag.Lookup("test").Value.String()
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		fmt.Printf("Error getting home directory: %v\n", err)
		os.Exit(1)
	}
	baseDir := filepath.Join(homeDir, ".tgup")
	cacheDir := filepath.Join(baseDir, "cache")
	logDir := filepath.Join(baseDir, "logs")

	_ = os.MkdirAll(cacheDir, 0755)
	_ = os.MkdirAll(logDir, 0755)

	configPath := filepath.Join(baseDir, "config.conf")
	config, err := loadConfig(configPath)
	if err != nil {
		fmt.Printf("Error loading config from %s: %v\n", configPath, err)
		os.Exit(1)
	}

	files, err := collectFiles(targetPath)
	if err != nil {
		fmt.Printf("Error accessing path: %v\n", err)
		os.Exit(1)
	}
	if len(files) == 0 {
		fmt.Println("No valid files found to upload.")
		return
	}

	if finalTestMode != "curl" {
		fmt.Printf("Found %d file(s) to upload.\n", len(files))
	}

	globalCaption := strings.TrimSpace(titleFlag)
	if globalCaption == "" && finalTestMode != "curl" {
		reader := bufio.NewReader(os.Stdin)
		fmt.Print("Do you want to add a caption for this upload? (Press Enter to skip): ")
		input, _ := reader.ReadString('\n')
		globalCaption = strings.TrimSpace(input)
	}

	var bot *tgbotapi.BotAPI
	if finalTestMode != "curl" {
		bot, err = tgbotapi.NewBotAPIWithAPIEndpoint(config.BotAPI, config.TgAPIURL+"/bot%s/%s")
		if err != nil {
			fmt.Printf("Error initializing bot: %v\n", err)
			os.Exit(1)
		}
		bot.Debug = false
	}

	const batchSize = 10
	for i := 0; i < len(files); i += batchSize {
		end := i + batchSize
		if end > len(files) {
			end = len(files)
		}
		batch := files[i:end]

		if finalTestMode == "curl" {
			generateCurlCommand(config, batch, globalCaption, cacheDir, cacheForceFlag)
		} else {
			fmt.Printf("\n--- Preparing batch: %d to %d (Total: %d) ---\n", i+1, end, len(files))
			uploadMediaGroup(bot, config.ChatID, batch, cacheDir, globalCaption, config, logDir, cacheForceFlag)
		}
	}

	if finalTestMode != "curl" {
		fmt.Println("\nAll uploads completed successfully!")
	}
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
	const maxPixels = 19500000

	if totalPixels <= maxPixels {
		return photoPath, nil
	}

	ratio := math.Sqrt(float64(maxPixels) / float64(totalPixels))
	newWidth := int(float64(imgConfig.Width) * ratio)
	newHeight := int(float64(imgConfig.Height) * ratio)

	_, _ = file.Seek(0, 0)
	srcImg, _, err := image.Decode(file)
	if err != nil {
		return photoPath, err
	}

	dstImg := image.NewRGBA(image.Rect(0, 0, newWidth, newHeight))
	draw.CatmullRom.Scale(dstImg, dstImg.Bounds(), srcImg, srcImg.Bounds(), draw.Over, nil)

	hasher := sha1.New()
	hasher.Write([]byte(fmt.Sprintf("%s_%d_%d", photoPath, newWidth, newHeight)))
	tmpName := fmt.Sprintf("resized_20m_%s.jpg", hex.EncodeToString(hasher.Sum(nil)))
	tmpPath := filepath.Join(cacheDir, tmpName)

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

	err = jpeg.Encode(outFile, dstImg, &jpeg.Options{Quality: 95})
	if err != nil {
		return photoPath, err
	}

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
	hasMedia := false
	for _, file := range files {
		ext := strings.ToLower(filepath.Ext(file))
		if contains(cfg.PhotoExts, ext) || contains(cfg.VideoExts, ext) {
			hasMedia = true
			break
		}
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

	var mediaJSONArray []TgMediaItem
	var fileFormFields []string
	isFirstItem := true

	photoIdx, videoIdx, docIdx := 1, 1, 1

	for _, file := range files {
		ext := strings.ToLower(filepath.Ext(file))
		isPhoto := contains(cfg.PhotoExts, ext)
		isVideo := contains(cfg.VideoExts, ext)

		if hasMedia && !isVideo && !isPhoto {
			continue
		}

		currentCaption := ""
		if isFirstItem && globalCaption != "" {
			currentCaption = globalCaption
			isFirstItem = false
		}

		var itemType, attachKey string

		if isVideo {
			itemType = "video"
			attachKey = fmt.Sprintf("video%d", videoIdx)

			// WebDAV 终极融合提取：一个统一的本地化缓存路由器
			width, height, duration, thumbPath, isPortraitVideo, thumbErr := getVideoDataUnified(file, cacheDir, cacheForce)
			thumbFormKey := fmt.Sprintf("thumb_video%d", videoIdx)
			var thumbValue string

			if thumbErr == nil && thumbPath != "" {
				absThumbPath, _ := filepath.Abs(thumbPath)
				thumbValue = fmt.Sprintf("attach://%s", thumbFormKey)
				fileFormFields = append(fileFormFields, fmt.Sprintf("     -F \"%s=@%s\"", thumbFormKey, absThumbPath))
			} else if thumbErr != nil {
				fmt.Printf("[Debug Error] Unified process failed for %s: %v\n", filepath.Base(file), thumbErr)
			}

			if isPortraitVideo && width > height {
				width, height = height, width
			}

			mediaJSONArray = append(mediaJSONArray, TgMediaItem{
				Type:              itemType,
				Media:             fmt.Sprintf("attach://%s", attachKey),
				Caption:           currentCaption,
				Width:             width,
				Height:            height,
				Duration:          duration,
				SupportsStreaming: true,
				Thumb:             thumbValue,
			})
			videoIdx++

			absPath, _ := filepath.Abs(file)
			fileFormFields = append(fileFormFields, fmt.Sprintf("     -F \"%s=@%s\"", attachKey, absPath))

		} else if isPhoto {
			itemType = "photo"
			attachKey = fmt.Sprintf("photo%d", photoIdx)
			photoIdx++

			finalPhotoPath, _ := checkAndResizePhoto(file, cacheDir, cacheForce)
			absPath, _ := filepath.Abs(finalPhotoPath)

			mediaJSONArray = append(mediaJSONArray, TgMediaItem{
				Type:    itemType,
				Media:   fmt.Sprintf("attach://%s", attachKey),
				Caption: currentCaption,
			})
			fileFormFields = append(fileFormFields, fmt.Sprintf("     -F \"%s=@%s\"", attachKey, absPath))
		} else {
			itemType = "document"
			attachKey = fmt.Sprintf("doc%d", docIdx)
			docIdx++

			mediaJSONArray = append(mediaJSONArray, TgMediaItem{
				Type:    itemType,
				Media:   fmt.Sprintf("attach://%s", attachKey),
				Caption: currentCaption,
			})
			absPath, _ := filepath.Abs(file)
			fileFormFields = append(fileFormFields, fmt.Sprintf("     -F \"%s=@%s\"", attachKey, absPath))
		}
	}

	if len(mediaJSONArray) == 0 {
		return
	}

	mediaJSONBytes, _ := json.MarshalIndent(mediaJSONArray, "     ", "  ")
	mediaJSONStr := string(mediaJSONBytes)

	apiURL := fmt.Sprintf("%s/bot%s/sendMediaGroup", cfg.TgAPIURL, cfg.BotAPI)

	fmt.Println("\n--- Generated Test CURL Command (With Image Optimizer) ---")
	fmt.Printf("curl -X POST \"%s\" \\\n", apiURL)
	fmt.Printf("     -F \"chat_id=%d\" \\\n", cfg.ChatID)
	escapedMediaJSON := strings.ReplaceAll(mediaJSONStr, `"`, `\"`)
	fmt.Printf("     -F \"media=%s\" \\\n", escapedMediaJSON)
	fmt.Println(strings.Join(fileFormFields, " \\\n"))
	fmt.Println("----------------------------------------------------------------------")
}

func writeLog(logDir string, filename string, content string) {
	logPath := filepath.Join(logDir, filename)
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		fmt.Printf("Failed to write log to %s: %v\n", filename, err)
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

	cfg := &Config{
		PhotoExts: []string{".jpg", ".jpeg", ".png"},
		VideoExts: []string{".mp4", ".mkv", ".avi", ".mov", ".m4v"},
	}

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
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])

		switch key {
		case "CHAT_ID":
			id, _ := strconv.ParseInt(val, 10, 64)
			cfg.ChatID = id
		case "BOT_API":
			cfg.BotAPI = val
		case "TG_API_URL":
			cfg.TgAPIURL = val
		case "PHOTO_EXTs":
			exts := strings.Split(strings.ToLower(val), ",")
			for i, e := range exts {
				exts[i] = strings.TrimSpace(e)
			}
			if len(exts) > 0 && exts[0] != "" {
				cfg.PhotoExts = exts
			}
		case "VIDEO_EXTS":
			exts := strings.Split(strings.ToLower(val), ",")
			for i, e := range exts {
				exts[i] = strings.TrimSpace(e)
			}
			if len(exts) > 0 && exts[0] != "" {
				cfg.VideoExts = exts
			}
		}
	}
	return cfg, scanner.Err()
}

func collectFiles(targetPath string) ([]string, error) {
	fi, err := os.Stat(targetPath)
	if err != nil {
		return nil, err
	}

	var files []string
	if !fi.IsDir() {
		return []string{targetPath}, nil
	}

	err = filepath.Walk(targetPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			if strings.HasPrefix(info.Name(), ".") {
				return nil
			}
			files = append(files, path)
		}
		return nil
	})
	return files, err
}

func contains(slice []string, val string) bool {
	for _, item := range slice {
		itemLower := strings.ToLower(item)
		valLower := strings.ToLower(val)
		if itemLower == valLower {
			return true
		}
	}
	return false
}

func uploadMediaGroup(bot *tgbotapi.BotAPI, chatID int64, files []string, cacheDir string, globalCaption string, cfg *Config, logDir string, cacheForce bool) {
	hasMedia := false
	for _, file := range files {
		ext := strings.ToLower(filepath.Ext(file))
		if contains(cfg.PhotoExts, ext) || contains(cfg.VideoExts, ext) {
			hasMedia = true
			break
		}
	}

	var validFiles []string
	for _, file := range files {
		ext := strings.ToLower(filepath.Ext(file))
		isPhoto := contains(cfg.PhotoExts, ext)
		isVideo := contains(cfg.VideoExts, ext)
		if hasMedia && !isVideo && !isPhoto {
			fmt.Printf("[Skipped] %s (Cannot mix general Document with Photo/Video in an album)\n", filepath.Base(file))
			continue
		}
		validFiles = append(validFiles, file)
	}

	if len(validFiles) == 0 {
		return
	}

	type FilePayload struct {
		FieldKey     string
		FilePath     string
		ShowProgress bool
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

	var mediaJSONArray []TgMediaItem
	var payloads []FilePayload

	isFirstItem := true
	photoIdx, videoIdx, docIdx := 1, 1, 1

	for _, file := range validFiles {
		ext := strings.ToLower(filepath.Ext(file))
		isPhoto := contains(cfg.PhotoExts, ext)
		isVideo := contains(cfg.VideoExts, ext)

		currentCaption := ""
		if isFirstItem && globalCaption != "" {
			currentCaption = globalCaption
			isFirstItem = false
		}

		if isVideo {
			attachKey := fmt.Sprintf("video%d", videoIdx)
			// WebDAV 优化调用：从强化的本地统一缓存路由器拿取数据
			width, height, duration, thumbPath, isPortraitVideo, thumbErr := getVideoDataUnified(file, cacheDir, cacheForce)
			var thumbValue string

			if thumbErr == nil && thumbPath != "" {
				thumbFormKey := fmt.Sprintf("thumb_video%d", videoIdx)
				thumbValue = fmt.Sprintf("attach://%s", thumbFormKey)
				payloads = append(payloads, FilePayload{FieldKey: thumbFormKey, FilePath: thumbPath, ShowProgress: false})
			}

			if isPortraitVideo && width > height {
				width, height = height, width
			}

			mediaJSONArray = append(mediaJSONArray, TgMediaItem{
				Type:              "video",
				Media:             fmt.Sprintf("attach://%s", attachKey),
				Caption:           currentCaption,
				Width:             width,
				Height:            height,
				Duration:          duration,
				SupportsStreaming: true,
				Thumb:             thumbValue,
			})
			payloads = append(payloads, FilePayload{FieldKey: attachKey, FilePath: file, ShowProgress: true})
			videoIdx++

		} else if isPhoto {
			attachKey := fmt.Sprintf("photo%d", photoIdx)
			finalPhotoPath, _ := checkAndResizePhoto(file, cacheDir, cacheForce)

			mediaJSONArray = append(mediaJSONArray, TgMediaItem{
				Type:    "photo",
				Media:   fmt.Sprintf("attach://%s", attachKey),
				Caption: currentCaption,
			})
			payloads = append(payloads, FilePayload{FieldKey: attachKey, FilePath: finalPhotoPath, ShowProgress: true})
			photoIdx++
		} else {
			attachKey := fmt.Sprintf("doc%d", docIdx)
			mediaJSONArray = append(mediaJSONArray, TgMediaItem{
				Type:    "document",
				Media:   fmt.Sprintf("attach://%s", attachKey),
				Caption: currentCaption,
			})
			payloads = append(payloads, FilePayload{FieldKey: attachKey, FilePath: file, ShowProgress: true})
			docIdx++
		}
	}

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
			part, err := multipartWriter.CreateFormFile(p.FieldKey, filepath.Base(p.FilePath))
			if err != nil {
				return
			}
			file, err := os.Open(p.FilePath)
			if err != nil {
				return
			}

			if p.ShowProgress {
				fi, _ := file.Stat()
				bar := progressbar.NewOptions64(
					fi.Size(),
					progressbar.OptionSetDescription(fmt.Sprintf("[Uploading] %s", filepath.Base(p.FilePath))),
					progressbar.OptionSetWriter(os.Stderr),
					progressbar.OptionShowBytes(true),
					progressbar.OptionSetWidth(15),
					progressbar.OptionThrottle(65),
					progressbar.OptionShowCount(),
					progressbar.OptionOnCompletion(func() { fmt.Fprint(os.Stderr, "\n") }),
					progressbar.OptionSpinnerType(14),
					progressbar.OptionFullWidth(),
				)
				proxyReader := progressbar.NewReader(file, bar)
				_, _ = io.Copy(part, &proxyReader)
			} else {
				_, _ = io.Copy(part, file)
			}
			file.Close()
		}
	}()

	apiURL := fmt.Sprintf("%s/bot%s/sendMediaGroup", cfg.TgAPIURL, cfg.BotAPI)
	req, err := http.NewRequest("POST", apiURL, pipeReader)
	if err != nil {
		fmt.Printf("Failed to create request: %v\n", err)
		return
	}
	req.Header.Set("Content-Type", contentType)

	client := &http.Client{Timeout: 0}
	resp, err := client.Do(req)

	currentTimeStr := time.Now().Format("2006-01-02 15:04:05")
	fileListStr := strings.Join(validFiles, ", ")

	if err != nil {
		errLogContent := fmt.Sprintf("[%s] ERROR: %v | Files attempted: [%s]\n", currentTimeStr, err, fileListStr)
		writeLog(logDir, "error.log", errLogContent)
		fmt.Printf("Upload request failed: %v\n", err)
		return
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusOK {
		fmt.Println("Batch upload success.")
		okLogContent := fmt.Sprintf("[%s] SUCCESS: Status: %d | Files uploaded: [%s]\n", currentTimeStr, resp.StatusCode, fileListStr)
		writeLog(logDir, "ok.log", okLogContent)
	} else {
		fmt.Printf("\nTelegram API Error: Status %d, Body: %s\n", resp.StatusCode, string(respBody))
		errLogContent := fmt.Sprintf("[%s] ERROR: Status %d | Body: %s | Files attempted: [%s]\n", currentTimeStr, resp.StatusCode, string(respBody), fileListStr)
		writeLog(logDir, "error.log", errLogContent)
	}
}

// getVideoDataUnified WebDAV 环境终极加速路由器：统一合并本地化缓存层
// 命中缓存时，不仅直接返回本地 JPG，还将完全【跳过】ffprobe 云端网络调用，达成毫秒级零延迟。
func getVideoDataUnified(videoPath string, cacheDir string, cacheForce bool) (w, h, duration int, thumbPath string, isPortrait bool, err error) {
	hasherName := sha1.New()
	hasherName.Write([]byte(videoPath))
	token := hex.EncodeToString(hasherName.Sum(nil))

	finalThumbPath := filepath.Join(cacheDir, token+".jpg")
	finalMetaPath := filepath.Join(cacheDir, token+".json")

	isCacheValid := false
	var cachedMeta VideoMetaCache

	// 1. 如果没有开启强制刷新，优先审计本地多媒体强缓存
	if !cacheForce {
		_, errThumb := os.Stat(finalThumbPath)
		metaBytes, errMeta := os.ReadFile(finalMetaPath)

		if errThumb == nil && errMeta == nil {
			if json.Unmarshal(metaBytes, &cachedMeta) == nil {
				// 深度核验缓存合法性，确保尺寸完全契合 TG 知识库 320px 的硬限制
				tW, tH := getImageDimensions(finalThumbPath)
				if tW > 0 && tH > 0 && tW <= 320 && tH <= 320 {
					isCacheValid = true
				}
			}
		}
	}

	// 2. 缓存命中：直接返回，对 WebDAV 达到 0 字节网络消耗
	if isCacheValid {
		return cachedMeta.Width, cachedMeta.Height, cachedMeta.Duration, finalThumbPath, cachedMeta.IsPortrait, nil
	}

	// 3. 缓存失效：强制执行物理洗盘重构
	_ = os.Remove(finalThumbPath)
	_ = os.Remove(finalMetaPath)

	// A. 发起云端 ffprobe 获取基础像素参数
	cmdProbe := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries", "stream=width,height,duration", "-of", "default=noprint_wrappers=1", videoPath)
	outProbe, err := cmdProbe.Output()
	if err != nil {
		return 0, 0, 0, "", false, fmt.Errorf("ffprobe cloud query failed: %v", err)
	}
	scanner := bufio.NewScanner(strings.NewReader(string(outProbe)))
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

	// B. 发起极简云端单帧抽帧（锁定第1秒，规避大范围随机寻道崩溃）
	rawTempJpg := filepath.Join(cacheDir, "raw_extracted_frame.jpg")
	_ = os.Remove(rawTempJpg)
	cmdImg := exec.Command("ffmpeg", "-y", "-i", videoPath, "-ss", "00:00:01", "-vframes", "1", "-q:v", "2", rawTempJpg)
	if err := cmdImg.Run(); err != nil {
		cmdFallback := exec.Command("ffmpeg", "-y", "-i", videoPath, "-vframes", "1", "-q:v", "2", rawTempJpg)
		if errFallback := cmdFallback.Run(); errFallback != nil {
			return 0, 0, 0, "", false, fmt.Errorf("ffmpeg frame extraction failed: %v", errFallback)
		}
	}
	defer os.Remove(rawTempJpg)

	// C. 收回到本地内存处理 320px 等比缩放与横竖判定
	rawFile, err := os.Open(rawTempJpg)
	if err != nil {
		return 0, 0, 0, "", false, err
	}
	srcImg, _, err := image.Decode(rawFile)
	rawFile.Close()
	if err != nil {
		return 0, 0, 0, "", false, err
	}

	bounds := srcImg.Bounds()
	srcW := bounds.Dx()
	srcH := bounds.Dy()

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
	if newW == 0 {
		newW = 2
	}
	if newHeight == 0 {
		newHeight = 2
	}

	dstImg := image.NewRGBA(image.Rect(0, 0, newW, newHeight))
	draw.CatmullRom.Scale(dstImg, dstImg.Bounds(), srcImg, srcImg.Bounds(), draw.Over, nil)

	outFile, err := os.Create(finalThumbPath)
	if err != nil {
		return 0, 0, 0, "", false, err
	}
	err = jpeg.Encode(outFile, dstImg, &jpeg.Options{Quality: 92})
	outFile.Close()
	if err != nil {
		return 0, 0, 0, "", false, err
	}

	if newHeight > newW {
		isPortrait = true
	}

	// D. 存储元数据到本地本地本地本地 JSON，完成双重闭环缓存
	newCacheData := VideoMetaCache{
		Width:      w,
		Height:     h,
		Duration:   duration,
		IsPortrait: isPortrait,
	}
	metaBytes, _ := json.Marshal(newCacheData)
	_ = os.WriteFile(finalMetaPath, metaBytes, 0644)

	return w, h, duration, finalThumbPath, isPortrait, nil
}
