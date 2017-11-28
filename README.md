# monostat-influxdb-monitor
this is a use golang to monitor monodb and insert data to influxdb

conf 里面需要配置字段名称，需要跟你mongostat字段顺序一致，对qr/qw ar/aw进行了拆分

这个可以用来流失的监控整个mongo一个数据集合，使用grafana达到可视化监控与报警
