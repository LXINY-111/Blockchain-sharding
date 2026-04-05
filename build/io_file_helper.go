package build

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"os"
)

// string2uint64 应该在 tool.go 中定义，如果没有，请确保 tool.go 存在
func readIpTable(ipTablePath string) map[uint64]map[uint64]string {
	// Read the contents of ipTable.json
	file, err := os.Open(ipTablePath)
	if err != nil {
		fmt.Println(err)
		log.Panic(err)
	}
	defer file.Close()

	byteValue, _ := ioutil.ReadAll(file)

	// 先解析为 string map，因为 JSON key 只能是 string
	var result map[string]map[string]string
	err = json.Unmarshal(byteValue, &result)
	if err != nil {
		fmt.Println(err)
		log.Panic(err)
	}

	// 转换为 uint64 map
	ipMap := make(map[uint64]map[uint64]string)
	for shardIDStr, nodes := range result {
		sid := string2uint64(shardIDStr) // 这个函数在 tool.go 里
		ipMap[sid] = make(map[uint64]string)
		for nodeIDStr, ip := range nodes {
			nid := string2uint64(nodeIDStr)
			ipMap[sid][nid] = ip
		}
	}
	return ipMap
}

// make sure the file is uptodate
var fileUpToDataSet = make(map[string]struct{})

func attachLineToFile(filePath string, line string) error {
	if _, exist := fileUpToDataSet[filePath]; !exist {
		if delErr := os.Remove(filePath); delErr != nil && !os.IsNotExist(delErr) {
			log.Panic(delErr)
		}
		fileUpToDataSet[filePath] = struct{}{}
	}
	// 以追加模式打开文件，如果文件不存在则创建
	file, err := os.OpenFile(filePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer file.Close()

	// 写入文件的内容，附加一个换行符
	if _, err := file.WriteString(line + "\n"); err != nil {
		return err
	}

	return nil
}
