package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/schollz/progressbar/v3"
)

type FilePayload struct {
	FieldKey      string `json:"field_key"`
	FilePath      string `json:"file_path"`
	ShowProgress  bool   `json:"show_progress"`
}

// TgMediaItem 🟢 彻底修复：严格独立分行，绝不混用 Tag 占位
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

func preProcessAndUpload(bot *tgbotapi.BotAPI, config *Config, batch []string, cacheDir string, cacheForceFlag bool, globalCaption string, logDir string, finalTestMode string) {
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
		uploadMediaGroup(bot, config.ChatID, batch, cacheDir, globalCaption, config, logDir, cacheForceFlag)
	}
}

func handleSplitVideoUpload(bot *tgbotapi.BotAPI, cfg *Config, origPath string, cacheDir string, cacheForce bool, globalCaption string, logDir string, finalTestMode string, batchSize int, sleepDuration int) {
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
					fmt.Printf("🎉 [元数据强缓存命中] 该超大视频已成功投递完毕，直接跳过！\n文件：%s\n", baseName)
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
	fmt.Printf("📦 切片大功告成！裂变生成 %d 个分段小视频，开始进入分批画廊队列...\n", len(splitPieces))

	chunks := chunkBySizeAndCount(splitPieces, batchSize, math.MaxInt64)
	processed := 0
	for ci, pieceBatch := range chunks {
		fmt.Printf("🎬 正在投递大视频切片子集专辑 (%d-%d / 总数 %d)...\n", processed+1, processed+len(pieceBatch), len(splitPieces))
		preProcessAndUpload(bot, cfg, pieceBatch, cacheDir, cacheForce, globalCaption, logDir, finalTestMode)
		processed += len(pieceBatch)

		if ci < len(chunks)-1 && sleepDuration > 0 {
			time.Sleep(time.Duration(sleepDuration) * time.Second)
		}
	}

	fmt.Println("🧹 上传结束，正在清刷本地大体积视频缓存大文件...")
	_ = os.Remove(localOrgPath)
	for _, piece := range splitPieces {
		_ = os.Remove(piece)
		hasherName := sha1.New()
		hasherName.Write([]byte(piece))
		_ = os.Remove(filepath.Join(cacheDir, hex.EncodeToString(hasherName.Sum(nil))+".jpg"))
	}
	cacheData.Uploaded = true
	metaBytes, _ := json.MarshalIndent(cacheData, "", "  ")
	_ = os.WriteFile(jsonPath, metaBytes, 0644)
	fmt.Println("✨ 本地大视频缓存已全数粉碎，元数据成功留存。")
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

func uploadMediaGroup(bot *tgbotapi.BotAPI, chatID int64, files []string, cacheDir string, globalCaption string, cfg *Config, logDir string, cacheForce bool) {
	if cfg.RateLimit > 0 {
		waitRateLimit(cfg.RateLimit)
	}

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
				payloads = append(payloads, FilePayload{FieldKey: thumbFormKey, FilePath: thumbPath, ShowProgress: false})
			}
			if isPortraitVideo && width > height {
				width, height = height, width
			}
			mediaJSONArray = append(mediaJSONArray, TgMediaItem{Type: "video", Media: "attach://" + attachKey, Caption: currentCaption, Width: width, Height: height, Duration: duration, SupportsStreaming: true, Thumb: thumbValue})
			payloads = append(payloads, FilePayload{FieldKey: attachKey, FilePath: file, ShowProgress: true})
			videoIdx++
		} else if isPhoto {
			attachKey := fmt.Sprintf("photo%d", photoIdx)
			finalPhotoPath, _ := checkAndResizePhoto(file, cacheDir, cacheForce)
			mediaJSONArray = append(mediaJSONArray, TgMediaItem{Type: "photo", Media: "attach://" + attachKey, Caption: currentCaption})
			payloads = append(payloads, FilePayload{FieldKey: attachKey, FilePath: finalPhotoPath, ShowProgress: true})
			photoIdx++
		} else {
			attachKey := fmt.Sprintf("doc%d", docIdx)
			mediaJSONArray = append(mediaJSONArray, TgMediaItem{Type: "document", Media: "attach://" + attachKey, Caption: currentCaption})
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
				uploadFileName := filepath.Base(p.FilePath)
				part, err := multipartWriter.CreateFormFile(p.FieldKey, uploadFileName)
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
					bar := progressbar.NewOptions64(fi.Size(), progressbar.OptionSetDescription(fmt.Sprintf("[Uploading] %s", descStr)), progressbar.OptionSetWriter(os.Stderr), progressbar.OptionShowBytes(true), progressbar.OptionSetWidth(15), progressbar.OptionThrottle(65), progressbar.OptionShowCount(), progressbar.OptionOnCompletion(func() { fmt.Fprint(os.Stderr, "\n") }), progressbar.OptionSpinnerType(14), progressbar.OptionFullWidth())

					proxyReader := progressbar.NewReader(file, bar)
					_, _ = io.Copy(part, &proxyReader)
				} else {
					_, _ = io.Copy(part, file)
				}
				file.Close()
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
			fmt.Printf("❌ [网络传输失败] 连接遭服务器强行中断（多半单次发包体积超标）: %v\n", err)
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

func writeLog(logDir string, filename string, content string) {
	f, err := os.OpenFile(filepath.Join(logDir, filename), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(content)
}

var requestTimestamps []time.Time

func waitRateLimit(limit int) {
	for {
		now := time.Now()
		cutoff := now.Add(-60 * time.Second)

		valid := 0
		for _, t := range requestTimestamps {
			if t.After(cutoff) {
				requestTimestamps[valid] = t
				valid++
			}
		}
		requestTimestamps = requestTimestamps[:valid]

		if len(requestTimestamps) < limit {
			break
		}

		oldest := requestTimestamps[0]
		waitDuration := oldest.Add(60 * time.Second).Sub(now)
		if waitDuration > 0 {
			fmt.Printf("\n⏳ 触发群组发包限频控制（每分钟最多 %d 个请求）。自主休眠等待 %.1f 秒...\n", limit, waitDuration.Seconds())
			time.Sleep(waitDuration)
		}
	}
	requestTimestamps = append(requestTimestamps, time.Now())
}

