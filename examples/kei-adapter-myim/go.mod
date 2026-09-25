module github.com/example/kei-adapter-myim

go 1.25.0

require github.com/RandomLemon/kei v0.0.0

// 本地开发用：真实第三方仓库把它换成 require 的版本号后删除本行。
replace github.com/RandomLemon/kei => ../..
