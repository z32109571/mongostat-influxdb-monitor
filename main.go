package main

import (
	"bufio"
	"flag"
	"io"
	"net"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/influxdata/influxdb/client"
)

var (
	pConfig ProxyConfig
	//	pLog       *logrus.Logger
	configFile = flag.String("c", "conf/conf.ini", "config file,default conf/conf.ini")
)

func main() {
	flag.Parse()
	err := parseConfigFile(*configFile)
	if err != nil {
		log.Warn(err)
	}
	log.Info("start monitor")
	args := strings.Fields(pConfig.Args)
	log.Info(args)
	cmd := exec.Command(pConfig.Command, args...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Warn(err)
	}
	err = cmd.Start()
	if err != nil {
		log.Warn(err)
		log.Warn(cmd.Stderr)
	}
	reader := bufio.NewReader(stdout)
	host, err := url.Parse(pConfig.InfluxUrl)
	con, err := client.NewClient(client.Config{URL: *host})
	if err != nil {
		log.Warn(err)
	}
	columes := strings.Split(pConfig.Colume, ",")
	log.Info(columes)
	ip := get_internal()
	pts := make([]client.Point, 1)
	lenColums := len(columes)
	for {
		line, err := reader.ReadString('\n')
		if err != nil || io.EOF == err {
			log.Warn(err)
			continue
		}
		s := strings.Fields(line)
		if len(s)+3 < lenColums {
			continue
		}
		log.Info(s)
		tags := make(map[string]string)
		fileds := make(map[string]interface{})
		iTag := 0
		for index, value := range s {
			tags["host"] = ip
			tags["port"] = pConfig.Port
			value = strings.Trim(value, "*")
			if columes[index+2] == "set" || columes[index+2] == "repl" || columes[index+2] == "time" {
				tags[columes[index+2]] = value
				continue
			}
			if columes[index] == "locked-db" {
				c := strings.Split(value, ":")
				fileds["locked-db-name"] = c[0]
				i := strings.Trim(c[1], "%")
				data, err := strconv.ParseFloat(i, 32)
				if err != nil {
					log.Warn(err)
				}
				fileds["locked-db-precent"] = data
				continue
			} else if columes[index] == "qr" {
				iTag = index + 3
				c := strings.Split(value, "|")
				data1, err := strconv.Atoi(c[0])
				if err != nil {
					log.Warn(err)
				}
				fileds["qr"] = data1
				data2, err := strconv.Atoi(c[1])
				if err != nil {
					log.Warn(err)
				}
				fileds["qw"] = data2
				continue
			} else if columes[index+1] == "ar" {
				c := strings.Split(value, "|")
				data1, err := strconv.Atoi(c[0])
				if err != nil {
					log.Warn(err)
				}
				fileds["ar"] = data1
				data2, err := strconv.Atoi(c[1])
				if err != nil {
					log.Warn(err)
				}
				fileds["aw"] = data2
				continue
			} else if columes[index] == "command" {
				if strings.Contains(value, "|") {
					c := strings.Split(value, "|")
					data1, err := strconv.Atoi(c[0])
					if err != nil {
						log.Warn(err)
					}
					fileds["command-local"] = data1
					data2, err := strconv.Atoi(c[1])
					if err != nil {
						log.Warn(err)
					}
					fileds["command-replicated"] = data2
					fileds["command"] = data1 + data2
					continue
				} else {
					data, err := strconv.Atoi(value)
					if err != nil {
						log.Warn(err)
					}
					fileds["command"] = data
					continue
				}
			} else {
				if iTag > 0 && index+2 > iTag {
					fileds[columes[index+2]] = unixtoFloat(value)
				} else {
					fileds[columes[index]] = unixtoFloat(value)
				}
				continue
			}
		}
		pts[0] = client.Point{
			Measurement: pConfig.Table,
			Tags:        tags,
			Fields:      fileds,
			Time:        time.Now(),
			Precision:   "ns",
		}
		bps := client.BatchPoints{
			Points:          pts,
			Database:        pConfig.Db,
			RetentionPolicy: pConfig.Rp,
		}
		log.Infof("bps is %v", bps)
		_, err = con.Write(bps)
		if err != nil {
			log.Warn(err)
		}
	}
	cmd.Wait()
}

func get_internal() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		log.Warn(err)
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ipnet.IP.To4() != nil {
				return ipnet.IP.String()
			}
		}
	}
	return ""
}

func unixtoFloat(value string) float64 {
	if strings.Contains(value, "g") ||
		strings.Contains(value, "G") {
		value = strings.Trim(value, "g")
		value = strings.Trim(value, "G")
		i, err := strconv.ParseFloat(value, 64)
		if err != nil {
			log.Warn(err)
		}
		return i * 1024 * 1024 * 1024
	} else if strings.Contains(value, "m") ||
		strings.Contains(value, "M") {
		value = strings.Trim(value, "m")
		value = strings.Trim(value, "M")
		i, err := strconv.ParseFloat(value, 64)
		if err != nil {
			log.Warn(err)
		}
		return i * 1024 * 1024
	} else if strings.Contains(value, "k") ||
		strings.Contains(value, "K") {
		value = strings.Trim(value, "k")
		value = strings.Trim(value, "K")
		i, err := strconv.ParseFloat(value, 64)
		if err != nil {
			log.Warn(err)
		}
		return i * 1024
	} else if strings.Contains(value, "b") ||
		strings.Contains(value, "B") {
		value = strings.Trim(value, "b")
		value = strings.Trim(value, "B")
		i, err := strconv.ParseFloat(value, 64)
		if err != nil {
			log.Warn(err)
		}
		return i
	}
	i, err := strconv.ParseFloat(value, 64)
	if err != nil {
		log.Warn(err)
	}
	return i
}
