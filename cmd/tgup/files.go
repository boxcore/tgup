package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
)

type fileSortItem struct {
	path    string
	modTime time.Time
	creTime time.Time
	size    int64
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
					secField, nsecField := statField.FieldByName("Sec"), statField.FieldByName("Nsec")
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

func contains(slice []string, val string) bool {
	for _, item := range slice {
		if strings.EqualFold(item, val) {
			return true
		}
	}
	return false
}

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
