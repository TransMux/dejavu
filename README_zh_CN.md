# DejaVu

[English](README.md)

**状态：开发中**

## 💡 简介

[DejaVu](https://github.com/siyuan-note/dejavu) 是一个用于数据快照和同步的 golang 库。

## ✨ 特性

* 类似 Git 的版本控制
* 文件分块去重
* 数据压缩
* AES 加密
* 连接云端存储

⚠️ 注意

* 不支持文件夹
* 不支持权限属性
* 不支持符号链接

## 🎨 设计

设计参考自 [ArtiVC](https://github.com/InfuseAI/ArtiVC)。

### 实体

* `ID` 每个实体都通过 SHA-1 标识
* `Index` 文件列表，每次索引操作都生成一个新的索引
    * `parent` 指向上一个索引
    * `memo` 索引备注
    * `created` 索引时间
    * `files` 文件列表
    * `size` 文件列表总大小
* `File` 文件，实际的数据文件路径或者内容发生变动时生成一个新的文件
    * `path` 文件路径
    * `size` 文件大小
    * `updated` 最后更新时间
    * `chunks` 文件分块列表
* `Chunk` 文件块
    * `data` 实际的数据
* `Ref` 引用指向索引
    * `latest` 内置引用，自动指向最新的索引
    * `tag` 标签引用，手动指向指定的索引
* `Repo` 仓库

### 仓库

* `DataPath` 数据文件夹路径，实际的数据文件所在文件夹
* `Path` 仓库文件夹路径，仓库不保存在数据文件夹中，需要单独指定仓库文件夹路径

仓库文件夹结构如下：

```text
+---objects
|   +---00
|   |       e605c489491e553ef60eb13911cd1446ac6a0d
|   |
|   +---01
|   |       453ac7651a523eda839a0ef9b4d653f884c84a
|   |       cca40956df1159e8bbb56724cc80aca5fe378c
|   |       e58c0ae476b3fd79630e118e05527fc0a4ae54
|   |
|   +---03
|   |       0c904b32935936cafada2f54b6cfe3d02b2080
|   |       d61fb01c9abf0e6ec1279f98a3f1abfadcbfad
|   |       d6a0e2fba3b8d97539b9a54865d4e4f18b4a2f
|   |
|   +---08
|   |       8f46fa8bd3af4a32d17e80c93c849435d8e703
|   |
|   +---09
|   |       03cba26bd73c8849b750a07e19624f51df02ad
|   |       8fe907ab51c47082a83d0086d820aa1750c8a9
|   |
|   +---0a
|   |       7a7d148d34c87c344b1aa86edeef5242b5db6f
|   |       da61c49a77d61a7db9ef48def08b61311cff8b
|   \---ff
|           3d40c741e5a8491e578e93a5c20e054941ea07
|           41a69dd2283707cbd7baba1db6c8ce8116c9b5
|           ab8b9036fe7fabf3281d25de8c335cfbd77229
\---refs
    |   latest
    |
    \---tags
            v1.0.0
            v1.0.1
```

### 同步合并

TBD

## 📄 授权

DejaVu 使用 [木兰宽松许可证, 第2版](http://license.coscl.org.cn/MulanPSL2) 开源协议。

## 🙏 鸣谢

* [ArtiVC](https://github.com/InfuseAI/ArtiVC)
* [restic](https://github.com/restic/restic)
