package main

import (
	//"fmt"

	"github.com/goini"
)

// ProxyConfig Type
type ProxyConfig struct {
	InfluxUrl string
	Db        string
	Table     string
	Rp        string
	Command   string
	Colume    string
	Args      string
	Port      string
}

func parseConfigFile(filepath string) error {
	conf := goini.SetConfig(filepath)
	pConfig.InfluxUrl = conf.GetValue("influxdb", "url")
	pConfig.Db = conf.GetValue("influxdb", "db")
	pConfig.Table = conf.GetValue("influxdb", "table")
	pConfig.Rp = conf.GetValue("influxdb", "rp")
	pConfig.Colume = conf.GetValue("influxdb", "colume")
	pConfig.Port = conf.GetValue("command", "port")
	pConfig.Command = conf.GetValue("command", "command")
	pConfig.Args = conf.GetValue("command", "args")
	return nil
}
