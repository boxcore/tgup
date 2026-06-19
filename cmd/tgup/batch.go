package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// maxGroupBytes 是 Telegram sendMediaGroup 单次相册建议的总体积熔断阈值，
// 同时也是判定"超限大视频需要走切片专属流程"的阈值。
const maxGroupBytes int64 = 2000 * 1024 * 1024

// runItem 表示按文件原始顺序切出的一段：
// 要么是一组可以打包进同一个相册的普通文件（normalFiles），
// 要么是一个需要走独立切片上传流程的超限大视频（largeVideoPath）。
type runItem struct {
	largeVideoPath string
	normalFiles    []string
}

// buildRuns 按原始顺序扫描已过滤的文件列表，把连续的普通文件聚合成一段，
// 遇到超限大视频则单独切出一段。预检（list 模式）和真实上传共用这一份逻辑，
// 保证"预览看到的批次"和"实际发出去的批次"永远一致。
func buildRuns(files []string, cfg *Config) []runItem {
	var runs []runItem
	var buf []string
	flushBuf := func() {
		if len(buf) > 0 {
			runs = append(runs, runItem{normalFiles: buf})
			buf = nil
		}
	}
	for _, file := range files {
		ext := strings.ToLower(filepath.Ext(file))
		fi, err := os.Stat(file)
		if err != nil {
			continue
		}
		isLargeVideo := contains(cfg.VideoExts, ext) && fi.Size() > maxGroupBytes
		if isLargeVideo {
			if !cfg.AllowSpilFile {
				fmt.Printf("⚠️ 跳过大视频文件 (未激活切片开关 ALLOW_SPIL_FILE): %s (%s)\n", filepath.Base(file), formatSize(fi.Size()))
				continue
			}
			if fi.Size() > cfg.AllowMaxSize {
				fmt.Printf("❌ 超过最大文件尺寸限制 (ALLOW_MAX_SIZE: %s)，强制跳过: %s (%s)\n", formatSize(cfg.AllowMaxSize), filepath.Base(file), formatSize(fi.Size()))
				continue
			}
			flushBuf()
			runs = append(runs, runItem{largeVideoPath: file})
		} else {
			buf = append(buf, file)
		}
	}
	flushBuf()
	return runs
}

// chunkBySizeAndCount 把一段普通文件按 batchSize（同时也是 Telegram 相册单次上限 10）
// 和总体积阈值切块。关键修复点：Telegram 的 sendMediaGroup 要求每次 2-10 个素材，
// 如果按数量/体积切完后最后一批恰好只剩 1 个文件，这个请求会被 Telegram 直接拒绝，
// 导致"实际发出去的上传次数"和预期不一致。这里在切块完成后做一次"借位"修正：
// 如果最后一批只有 1 个文件，就从上一批末尾挪 1 个过来，凑成 2 个，避免孤儿批次。
func chunkBySizeAndCount(files []string, batchSize int, sizeThreshold int64) [][]string {
	var batches [][]string
	var current []string
	var currentSize int64

	flush := func() {
		if len(current) > 0 {
			batches = append(batches, current)
			current = nil
			currentSize = 0
		}
	}

	for _, file := range files {
		var size int64
		if fi, err := os.Stat(file); err == nil {
			size = fi.Size()
		}
		if len(current) > 0 && currentSize+size > sizeThreshold {
			flush()
		}
		current = append(current, file)
		currentSize += size
		if len(current) == batchSize {
			flush()
		}
	}
	flush()

	for {
		changed := false
		var clean [][]string
		for _, b := range batches {
			if len(b) > 0 {
				clean = append(clean, b)
			}
		}
		batches = clean

		if len(batches) < 2 {
			break
		}

		for i := 0; i < len(batches); i++ {
			if len(batches[i]) == 1 {
				if i == 0 {
					nextLen := len(batches[1])
					if nextLen+1 <= batchSize {
						batches[1] = append(batches[0], batches[1]...)
						batches[0] = nil
					} else {
						moved := batches[1][0]
						batches[1] = batches[1][1:]
						batches[0] = append(batches[0], moved)
					}
					changed = true
					break
				} else {
					prevLen := len(batches[i-1])
					if prevLen+1 <= batchSize {
						batches[i-1] = append(batches[i-1], batches[i]...)
						batches[i] = nil
					} else {
						moved := batches[i-1][prevLen-1]
						batches[i-1] = batches[i-1][:prevLen-1]
						batches[i] = append([]string{moved}, batches[i]...)
					}
					changed = true
					break
				}
			}
		}
		if !changed {
			break
		}
	}
	return batches
}
