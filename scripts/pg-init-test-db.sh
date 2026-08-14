#!/bin/sh
# 容器首次初始化时建出 go test 用的库（测试会在里面建/删独立 schema）。
set -e
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname postgres <<SQL
CREATE DATABASE gateway_test OWNER $POSTGRES_USER;
SQL
