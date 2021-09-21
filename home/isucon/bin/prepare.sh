#!/bin/sh

set -ex

(
  cd /home/isucon/webapp/go
  make
)

# mysql
sudo mysql -e "CALL sys.ps_truncate_all_tables(FALSE)"
#sudo rm -f /var/log/mysql/mysql-slow.log
#sudo mysqladmin flush-logs

# app
sudo systemctl restart isucholar.go

# nginx
sudo rm -f /var/log/nginx/access.log
sudo systemctl reload nginx

# result
rm -f /result/*.*
