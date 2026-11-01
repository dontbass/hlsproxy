module("luci.controller.hlsproxy", package.seeall)
function index()
    entry({"admin", "services", "hlsproxy"}, template("hlsproxy/index"), _("HLS Proxy (Анти-фриз)"), 60)
end
