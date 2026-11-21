// Applied before first paint so a saved theme never flashes.
try { var t = localStorage.getItem("aa.theme"); if (t === "light" || t === "dark") document.documentElement.dataset.theme = t; } catch (e) {}
