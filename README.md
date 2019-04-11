# deepin-pr-test

## 安装方法
```shell
wget -o pr-test.tar.xz https://github.com/electricface/deepin-pr-test/releases/download/latest/pr-test.tar.xz
tar axf pr-test.tar.xz
sudo mv -v pr-test /usr/local/bin
```

## 使用方法

### 安装 

安装 pull request 测试时在 jenkins 上打包出的 deb 包。
```
# 指定 pull request 的 url， 如：
pr-test https://github.com/linuxdeepin/startdde/pull/36

# 指定 pull request 的简写，格式为 仓库名#数字，如：
pr-test startdde#36

# 如果你是开发者，在 startdde 的代码目录中，只写数字也可以，如：
pr-test 36
```

### 查看状态
```
pr-test -status 
```
比如有如下输出：
```
Repo: startdde
Package: startdde
Title: chore: waiting for kwin after launch it
User: electricface
PR url: https://github.com/linuxdeepin/startdde/pull/36
Job url: https://ci.deepin.io/job/github-pr-check/16
```

### 恢复
```
# 恢复所有
pr-test -restore all

# 恢复某个用户的，看 -status 输出的 User 字段，比如
pr-test -restore electricface

# 恢复某个仓库的，看 -status 输出的 Repo 字段，比如：
pr-test -resotre startdde
```
