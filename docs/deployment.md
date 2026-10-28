# 序付 Docker 部署

正式源码：`https://github.com/yigerende/xufu.git`。目录 `/opt/1panel/apps/xufu`，Compose 项目 `xufu`，应用 `xufu-app`，数据库 `xufu-postgres`。对外端口 **5496**，容器内应用端口 18860；PostgreSQL 18 不发布宿主机端口，独立数据卷 `xufu_postgres-data` 和网络 `xufu_default`。

## 首次安装与数据迁移

```sh
git clone https://github.com/yigerende/xufu.git /opt/1panel/apps/xufu
cd /opt/1panel/apps/xufu
```

把本次迁移的生产 `.env` 通过私有通道保存到项目目录并设为 0600。保留原 `MASTER_KEY`、管理员信息和数据库凭据；否则原代理和账单资料无法解密。`.env.example` 只是新环境模板，不得覆盖现有配置。生产不设置 Windows 专用 `XUPAY_LOCAL_PROXY`，也不复制本机回环代理或 Chrome 路径。

先构建应用镜像，运行镜像内离线浏览器测试，确认无活动业务后停止本机应用，再使用 PostgreSQL 18 的 `pg_dump -Fc --no-owner --no-acl -n public` 最终导出。本机 PostgreSQL 保留为迁移备份，不再运行旧应用队列。

```sh
docker compose -p xufu build --build-arg SOURCE_REVISION="$(git rev-parse HEAD)" app
docker run --rm --network none --shm-size=1g xufu-app:local node --test runtime/payment-browser.test.mjs
docker compose -p xufu up -d postgres
# 仅首次、确认目标库为空时导入；此步骤禁止用于日常更新。
docker compose -p xufu exec -T postgres pg_restore -U xupay -d xupay --no-owner --no-acl --exit-on-error --single-transaction --clean --if-exists < /private/path/database.dump
```

首次导入的 `--clean --if-exists` 用来替换新空库自带的 public schema；必须先确认目标没有业务表，不能在已有正式数据的库上运行。数据库首次初始化会启动临时实例，须等待容器健康后再导入。

在应用启动前比对源库和目标库全部 public 表行数与内容指纹，并核对生产主密钥。成功后才启动应用：

```sh
docker compose -p xufu up -d --no-deps --no-build app
curl --fail http://127.0.0.1:5496/health/live
curl --fail http://127.0.0.1:5496/health/ready
```

本次使用 HTTP 端口访问，`COOKIE_SECURE=false`。后续接入 HTTPS 时再启用 secure cookie。不要开放数据库端口或信任任意客户端提供的转发 IP 头。

## 后续仅更新代码

1. 确认操作目录及 `docker compose -p xufu ps`。执行 `git pull --ff-only`；不要复制本机 `.env`、数据库或配置。
2. 保留当前应用镜像作回滚。旧应用继续运行期间，先构建新镜像并完成上述离线浏览器检查。`SOURCE_REVISION` 使用当前 Git 提交。
3. 用下方只读查询检查在途任务。非零时等待任务自然完成；不要中断付款、核实、续费或代理检测。
4. 把正式库 `pg_dump`、`.env`、`compose.yml`、Git 提交及旧镜像 ID 备份到 `/opt/1panel/apps/codex-release-backups/xufu/本次时间/`，目录权限 0700。验证 `pg_restore --list`，再次检查活动任务为零。
5. 仅切换应用，数据库保持原容器和数据卷：

```sh
docker compose -p xufu exec -T postgres psql -U xupay -d xupay -At < scripts/active-work.sql
docker compose -p xufu up -d --no-deps --no-build --force-recreate app
```

6. 检查 live/ready、前端资源、管理员登录、核心运行时与日志；核对其他项目容器 ID/启动时间未变化。主密钥、代理、账户及业务配置必须保留。成功后仅保留最近 3 份已验证的本项目完整备份。

禁止 `docker compose down -v`、删除数据卷、全局 prune、重放首次导入或用旧数据库备份覆盖新业务。回滚优先切回保留的旧应用镜像，先确认数据库兼容性。

## 运行与网络

- Node/Playwright/Chromium 随应用镜像安装，应用以 UID 10001 运行；共享内存 1GiB，init 回收浏览器子进程，日志滚动保留。
- 提链走后台订阅地区代理，关闭自动续费及前后查询走后台全局代理，付款运行时走服务器默认网络。数据库中的原地区/全局代理配置按原值迁移。
- `.env`、本机业务数据、迁移包、HAR 和密钥不进入 GitHub 或镜像。测试不发送真实付款或续费操作。
- 本机迁移标记 `.local/migrated-to-production.json` 会阻止 `start-local.ps1` 启动旧数据库的工作队列；后续本地开发应使用独立测试数据库。
