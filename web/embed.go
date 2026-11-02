// Package web 通过 go:embed 把静态设置页打进单文件 exe，运行时无需任何外部文件。
package web

import "embed"

// FS 内嵌全部前端资源（纯静态、离线可用、无外部 CDN）。
//
//go:embed index.html md3.css app.js icon-64.png icon-180.png
var FS embed.FS
