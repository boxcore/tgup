package main

import (
	"bufio"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/image/draw"
)

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

func getVideoDataUnified(videoPath string, cacheDir string, cacheForce bool) (w, h, duration int, thumbPath string, isPortrait bool, err error) {
	hasherName := sha1.New()
	hasherName.Write([]byte(videoPath))
	token := hex.EncodeToString(hasherName.Sum(nil))
	finalThumbPath, finalMetaPath := filepath.Join(cacheDir, token+".jpg"), filepath.Join(cacheDir, token+".json")

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
	cmdImg := exec.Command("ffmpeg", "-y", "-ss", "0", "-i", videoPath, "-vframes", "1", "-q:v", "2", rawTempJpg)
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

type VideoCheckInfo struct {
	Filename    string  `json:"filename"`
	FilePath    string  `json:"file_path"`
	Size        int64   `json:"size"`
	Width       int     `json:"width"`
	Height      int     `json:"height"`
	AspectRatio string  `json:"aspect_ratio"`
	Duration    float64 `json:"duration"`
	VideoCodec  string  `json:"video_codec"`
	AudioCodec  string  `json:"audio_codec"`
	CheckedAt   string  `json:"checked_at"`
}

type ffprobeStream struct {
	CodecType string `json:"codec_type"`
	CodecName string `json:"codec_name"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
}

type ffprobeFormat struct {
	Size     string `json:"size"`
	Duration string `json:"duration"`
}

type ffprobeJSON struct {
	Streams []ffprobeStream `json:"streams"`
	Format  ffprobeFormat   `json:"format"`
}

func checkVideoFiles(videoFiles []string, cacheDir string) {
	fmt.Printf("\n🔍 开始检测视频文件元数据并保存缓存 (共 %d 个视频)...\n", len(videoFiles))
	fmt.Println(strings.Repeat("=", 60))

	for idx, path := range videoFiles {
		fi, err := os.Stat(path)
		if err != nil {
			fmt.Printf("[%d/%d] ❌ 无法读取文件信息: %s, 错误: %v\n", idx+1, len(videoFiles), filepath.Base(path), err)
			continue
		}

		// Generate token using filename + size + modtime
		hasher := sha1.New()
		hasher.Write([]byte(path + strconv.FormatInt(fi.Size(), 10) + strconv.FormatInt(fi.ModTime().Unix(), 10)))
		token := hex.EncodeToString(hasher.Sum(nil))
		cacheJSONPath := filepath.Join(cacheDir, fmt.Sprintf("check_%s.json", token))

		// Execute ffprobe
		cmd := exec.Command("ffprobe", "-v", "error", "-show_format", "-show_streams", "-of", "json", path)
		out, err := cmd.Output()
		if err != nil {
			fmt.Printf("[%d/%d] ❌ ffprobe 探测失败: %s, 错误: %v\n", idx+1, len(videoFiles), filepath.Base(path), err)
			continue
		}

		var raw ffprobeJSON
		if err := json.Unmarshal(out, &raw); err != nil {
			fmt.Printf("[%d/%d] ❌ 解析 ffprobe JSON 失败: %s, 错误: %v\n", idx+1, len(videoFiles), filepath.Base(path), err)
			continue
		}

		// Extract fields
		var width, height int
		var videoCodec, audioCodec string
		for _, stream := range raw.Streams {
			if stream.CodecType == "video" {
				width = stream.Width
				height = stream.Height
				videoCodec = stream.CodecName
			} else if stream.CodecType == "audio" {
				audioCodec = stream.CodecName
			}
		}

		if videoCodec == "" {
			videoCodec = "n/a"
		}
		if audioCodec == "" {
			audioCodec = "none"
		}

		sizeVal, _ := strconv.ParseInt(raw.Format.Size, 10, 64)
		if sizeVal == 0 {
			sizeVal = fi.Size()
		}

		durationVal, _ := strconv.ParseFloat(raw.Format.Duration, 64)

		aspectRatio := getAspectRatio(width, height)
		var decimalAspect float64
		if height > 0 {
			decimalAspect = float64(width) / float64(height)
		}

		aspectStr := "N/A"
		if aspectRatio != "N/A" {
			aspectStr = fmt.Sprintf("%s (%.2f)", aspectRatio, decimalAspect)
		}

		info := VideoCheckInfo{
			Filename:    filepath.Base(path),
			FilePath:    path,
			Size:        sizeVal,
			Width:       width,
			Height:      height,
			AspectRatio: aspectRatio,
			Duration:    durationVal,
			VideoCodec:  videoCodec,
			AudioCodec:  audioCodec,
			CheckedAt:   time.Now().Format("2006-01-02 15:04:05"),
		}

		infoBytes, _ := json.MarshalIndent(info, "", "  ")
		if err := os.WriteFile(cacheJSONPath, infoBytes, 0644); err != nil {
			fmt.Printf("[%d/%d] ⚠️ 写入缓存文件失败: %v\n", idx+1, len(videoFiles), err)
		}

		// Check if streamable for TG warning suffix
		var warnSuffix string
		if !isTGStreamableWithCodec(path, info.VideoCodec) {
			warnSuffix = " ⚠️ [不支持TG流式点播]"
		}

		// Clean printable output
		durationStr := fmt.Sprintf("%.2f 秒 (%02d:%02d)", durationVal, int(durationVal)/60, int(durationVal)%60)
		fmt.Printf("[%d/%d] 🎬 检测视频: %s%s\n", idx+1, len(videoFiles), info.Filename, warnSuffix)
		fmt.Printf("      路径: %s\n", info.FilePath)
		fmt.Printf("      大小: %s\n", formatSize(info.Size))
		fmt.Printf("      分辨率: %dx%d (高宽比: %s)\n", info.Width, info.Height, aspectStr)
		fmt.Printf("      时长: %s\n", durationStr)
		fmt.Printf("      视频编码: %s\n", info.VideoCodec)
		fmt.Printf("      音频编码: %s\n", info.AudioCodec)
		fmt.Printf("      缓存路径: %s\n", cacheJSONPath)
		fmt.Println(strings.Repeat("-", 60))
	}
}

func getAspectRatio(w, h int) string {
	if w <= 0 || h <= 0 {
		return "N/A"
	}
	g := gcd(w, h)
	return fmt.Sprintf("%d:%d", w/g, h/g)
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

func isTGStreamableWithCodec(filePath string, videoCodec string) bool {
	ext := strings.ToLower(filepath.Ext(filePath))
	if ext != ".mp4" && ext != ".m4v" {
		return false
	}
	codec := strings.ToLower(videoCodec)
	return codec == "h264" || codec == "hevc" || codec == "h265" || codec == "avc"
}

func isTGStreamable(filePath string) (bool, string) {
	codec := getVideoCodec(filePath)
	return isTGStreamableWithCodec(filePath, codec), codec
}

func getVideoCodec(videoPath string) string {
	cmd := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries", "stream=codec_name", "-of", "default=noprint_wrappers=1", videoPath)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.Split(line, "=")
		if len(parts) == 2 && parts[0] == "codec_name" {
			return strings.TrimSpace(parts[1])
		}
	}
	return ""
}

func filterUnsupportedVideos(files []string, config *Config, cacheDir string) []string {
	var filtered []string
	for _, file := range files {
		ext := strings.ToLower(filepath.Ext(file))
		if !contains(config.VideoExts, ext) {
			// Keep non-videos (e.g. photos/documents)
			filtered = append(filtered, file)
			continue
		}

		streamable, codec := isTGStreamable(file)
		if streamable {
			filtered = append(filtered, file)
			continue
		}

		fmt.Println(strings.Repeat("=", 60))
		fmt.Printf("⚠️ 警告: 视频文件 [%s] (编码: %s) 在 Telegram 上不支持媒体流直接点播！\n", filepath.Base(file), codec)
		fmt.Println("提示: 建议提前将该视频转换为 H.264 MP4 格式后再行上传。")

		prompt := fmt.Sprintf("❓ 是否坚持直接上传该文件？(y/n, 5秒内无输入默认跳过): ")
		confirmed := askConfirmationWithTimeout(prompt, 5*time.Second)
		if confirmed {
			fmt.Println("✅ 已确认，将继续上传此视频。")
			filtered = append(filtered, file)
		} else {
			fmt.Printf("🚫 已跳过不支持的视频文件: %s\n", filepath.Base(file))
		}
		fmt.Println(strings.Repeat("=", 60))
	}
	return filtered
}

func askConfirmationWithTimeout(prompt string, timeout time.Duration) bool {
	fmt.Print(prompt)
	ch := make(chan string, 1)
	go func() {
		var input string
		_, err := fmt.Scanln(&input)
		if err != nil {
			ch <- ""
			return
		}
		ch <- strings.TrimSpace(strings.ToLower(input))
	}()

	select {
	case res := <-ch:
		return res == "y" || res == "yes"
	case <-time.After(timeout):
		fmt.Println("\n⏰ 5秒超时无响应，默认跳过。")
		return false
	}
}
