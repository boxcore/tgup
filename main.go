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
	// 1. 定义 Flags
	var titleFlag string
	var testFlag string
	var cacheForceFlag bool

	var batchSizeFlag int
	var typeFilterFlag string
	var sleepDurationFlag int

	flag.StringVar(&titleFlag, "title", "", "Specify a global caption title for the media group")
	flag.StringVar(&testFlag, "t", "", "Test mode: use '-t=curl' to print curl command")
	flag.StringVar(&testFlag, "test", "", "Test mode: use '--test=curl' to print curl command")
	flag.BoolVar(&cacheForceFlag, "cache-force", false, "Force regenerate and overwrite existing thumbnails/photos")
	flag.BoolVar(&cacheForceFlag, "cf", false, "Force regenerate and overwrite existing thumbnails/photos (shorthand)")

	flag.IntVar(&batchSizeFlag, "n", 10, "Batch size per media group (max 10)")
	flag.StringVar(&typeFilterFlag, "type", "all", "Filter media type: pic (photos only), video/vedio (videos only), all (mix)")
	flag.IntVar(&sleepDurationFlag, "s", 4, "Sleep duration in seconds between batch uploads")

	// 2. 官方标准解析
	flag.Parse()

	// 3. 获取剩余的位置参数（路径）
	tailArgs := flag.Args()
	if len(tailArgs) == 0 {
		fmt.Println("Error: Please specify a file or directory path.")
		fmt.Println("Usage: go run main.go [-n=10] [-type=video] [-s=5] <path>")
		os.Exit(1)
	}
	targetPath := tailArgs[0]

	// 4. 统一别名状态与规范防呆
	finalTestMode := testFlag
	if finalTestMode == "" {
		finalTestMode = flag.Lookup("test").Value.String()
	}

	if batchSizeFlag > 10 {
		batchSizeFlag = 10
	} else if batchSizeFlag <= 0 {
		batchSizeFlag = 10
	}
	if sleepDurationFlag < 0 {
		sleepDurationFlag = 0
	}

	// 5. 初始化环境与加载配置
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

	// 6. 解析目标路径获取文件列表
	rawFiles, err := collectFiles(targetPath)
	if err != nil {
		fmt.Printf("Error accessing path: %v\n", err)
		os.Exit(1)
	}

	// 7. 根据 -type 参数过滤文件列表
	var files []string
	typeFilterFlag = strings.ToLower(typeFilterFlag)
	for _, file := range rawFiles {
		ext := strings.ToLower(filepath.Ext(file))
		isPhoto := contains(config.PhotoExts, ext)
		isVideo := contains(config.VideoExts, ext)

		if typeFilterFlag == "pic" && !isPhoto {
			continue
		}
		if (typeFilterFlag == "video" || typeFilterFlag == "vedio") && !isVideo {
			continue
		}
		if !isPhoto && !isVideo {
			continue
		}
		files = append(files, file)
	}

	if len(files) == 0 {
		fmt.Println("No valid files found matching the specific type criteria.")
		return
	}

	if finalTestMode != "curl" {
		fmt.Printf("Found %d file(s) to process after filtering.\n", len(files))
	}

	// 8. 决定全局标题
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

	// 9. 动态分批处理
	for i := 0; i < len(files); i += batchSizeFlag {
		end := i + batchSizeFlag
		if end > len(files) {
			end = len(files)
		}
		batch := files[i:end]

		// 批次级单线程有序预处理，每完成一个强制休眠，绝不轰炸网盘
		if finalTestMode != "curl" {
			fmt.Printf("⚡ 正在有序预处理当前批次 (%d/%d) 的 WebDAV 缓存...\n", i+1, end)
			for _, file := range batch {
				ext := strings.ToLower(filepath.Ext(file))
				if contains(config.VideoExts, ext) {
					_, _, _, _, _, _ = getVideoDataUnified(file, cacheDir, cacheForceFlag)
					time.Sleep(600 * time.Millisecond)
				}
			}
		}

		if finalTestMode == "curl" {
			generateCurlCommand(config, batch, globalCaption, cacheDir, cacheForceFlag)
		} else {
			fmt.Printf("\n--- Preparing batch: %d to %d (Total: %d) ---\n", i+1, end, len(files))
			uploadMediaGroup(bot, config.ChatID, batch, cacheDir, globalCaption, config, logDir, cacheForceFlag)

			// 批次间物理隔离冷却
			if end < len(files) && sleepDurationFlag > 0 {
				fmt.Printf("🛋️ 批次发送完毕，主动进入 %d 秒战略冷却...\n", sleepDurationFlag)
				time.Sleep(time.Duration(sleepDurationFlag) * time.Second)
			}
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

		currentCaption := ""
		if isFirstItem && globalCaption != "" {
			currentCaption = globalCaption
			isFirstItem = false
		}

		var itemType, attachKey string

		if isVideo {
			itemType = "video"
			attachKey = fmt.Sprintf("video%d", videoIdx)

			width, height, duration, thumbPath, isPortraitVideo, thumbErr := getVideoDataUnified(file, cacheDir, cacheForce)
			thumbFormKey := fmt.Sprintf("thumb_video%d", videoIdx)
			var thumbValue string

			if thumbErr == nil && thumbPath != "" {
				absThumbPath, _ := filepath.Abs(thumbPath)
				thumbValue = fmt.Sprintf("attach://%s", thumbFormKey)
				fileFormFields = append(fileFormFields, fmt.Sprintf("     -F \"%s=@%s\"", thumbFormKey, absThumbPath))
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
		case "PHOTO_EXTS":
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

func uploadMediaGroup(bot *tgbotapi.BotAPI, chatID int64, files []string, cacheDir string, globalCaption string, cfg *Config, logDir string, cacheForce bool) {
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

	for _, file := range files {
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
			width, height, duration, thumbPath, isPortraitVideo, _ := getVideoDataUnified(file, cacheDir, cacheForce)
			var thumbValue string

			if thumbPath != "" {
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
					descStr := filepath.Base(p.FilePath)
					if retry > 0 {
						descStr = fmt.Sprintf("[重试-%d] %s", retry, descStr)
					}
					bar := progressbar.NewOptions64(
						fi.Size(),
						progressbar.OptionSetDescription(fmt.Sprintf("[Uploading] %s", descStr)),
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
		fileListStr := strings.Join(files, ", ")

		if err != nil {
			errLogContent := fmt.Sprintf("[%s] ERROR: %v | Files attempted: [%s]\n", currentTimeStr, err, fileListStr)
			writeLog(logDir, "error.log", errLogContent)
			fmt.Printf("Upload request failed: %v\n", err)
			return
		}

		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			fmt.Println("Batch upload success.")
			okLogContent := fmt.Sprintf("[%s] SUCCESS: Status: %d | Files uploaded: [%s]\n", currentTimeStr, resp.StatusCode, fileListStr)
			writeLog(logDir, "ok.log", okLogContent)
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

			fmt.Printf("\n⚠️ 触发 Telegram 官方频控限制。服务器返回: %s\n", bodyStr)
			fmt.Printf("💤 脚本将自动高精度休眠 %d 秒后，原地重构并自动重试当前批次 (%d/%d)...\n\n", waitSeconds+2, retry+1, maxRetries)
			time.Sleep(time.Duration(waitSeconds+2) * time.Second)
			continue
		}

		fmt.Printf("\nTelegram API Error: Status %d, Body: %s\n", resp.StatusCode, bodyStr)
		errLogContent := fmt.Sprintf("[%s] ERROR: Status %d | Body: %s | Files attempted: [%s]\n", currentTimeStr, resp.StatusCode, bodyStr, fileListStr)
		writeLog(logDir, "error.log", errLogContent)
		return
	}

	fmt.Println("\n❌ 当前批次已连续重试失败达到上限，放弃该批次。")
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

	// 【微操控流点 1】：避免瞬间密集请求，前置强制微休眠
	time.Sleep(200 * time.Millisecond)

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

	// 【微操控流点 2】：音视频流分离探测微歇
	time.Sleep(400 * time.Millisecond)

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

// contains 扩展名匹配切片辅助函数
func contains(slice []string, val string) bool {
	for _, item := range slice {
		if strings.EqualFold(item, val) {
			return true
		}
	}
	return false
}
