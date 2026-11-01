formatLocalTimes();

function formatLocalTimes() {
	const pad = (n) => String(n).padStart(2, "0");
	let tzAbbr = "";
	try {
		const parts = new Intl.DateTimeFormat(undefined, { timeZoneName: "short" }).formatToParts(new Date());
		const tzPart = parts.find((p) => p.type === "timeZoneName");
		if (tzPart && tzPart.value) tzAbbr = " " + tzPart.value;
	} catch (_) {}

	document.querySelectorAll(".local-time[data-utc]:not([data-formatted])").forEach((el) => {
		const raw = el.getAttribute("data-utc");
		if (!raw) return;
		const d = new Date(raw);
		if (isNaN(d.getTime())) {
			el.setAttribute("data-formatted", "1");
			return;
		}
		const y = d.getFullYear();
		const m = pad(d.getMonth() + 1);
		const day = pad(d.getDate());
		const hh = pad(d.getHours());
		const mm = pad(d.getMinutes());
		if (el.getAttribute("data-show-sec") === "1") {
			const ss = pad(d.getSeconds());
			el.textContent = y + "-" + m + "-" + day + " " + hh + ":" + mm + ":" + ss + tzAbbr;
		} else {
			el.textContent = y + "-" + m + "-" + day + " " + hh + ":" + mm;
		}
		el.setAttribute("data-formatted", "1");
	});
}

function copyFromBtn(btn) {
	const text = btn.getAttribute("data-copy");
	if (text) {
		copyText(text, btn);
	}
}

function copyText(text, btn) {
	const orig = btn.textContent;
	const markCopied = () => {
		btn.textContent = "Copied!";
		setTimeout(() => { btn.textContent = orig; }, 1400);
	};

	if (navigator.clipboard && navigator.clipboard.writeText) {
		navigator.clipboard.writeText(text).then(markCopied).catch(() => fallbackCopy(text, markCopied));
	} else {
		fallbackCopy(text, markCopied);
	}
}

function fallbackCopy(text, onDone) {
	const ta = document.createElement("textarea");
	ta.value = text;
	ta.style.position = "fixed";
	ta.style.opacity = "0";
	document.body.appendChild(ta);
	ta.focus();
	ta.select();
	try {
		if (document.execCommand("copy")) {
			onDone();
		}
	} finally {
		document.body.removeChild(ta);
	}
}

function changeSystem(sys) {
	const url = new URL(window.location.href);
	if (sys && sys !== "x86_64-linux") {
		url.searchParams.set("system", sys);
	} else {
		url.searchParams.delete("system");
	}
	window.location.href = url.pathname + url.search + url.hash;
}

function syncNonguixURLParam(checked) {
	if (!window.history || !window.history.replaceState) return;
	const url = new URL(window.location.href);
	if (checked) {
		url.searchParams.set("additional_channels", "nonguix");
	} else {
		url.searchParams.delete("additional_channels");
	}
	window.history.replaceState(null, "", url.pathname + url.search + url.hash);
	const homeLink = document.querySelector(".site-header-element-left a");
	if (homeLink) homeLink.setAttribute("href", "/" + url.search);
}

function toggleMainNonguix(checked) {
	const cliGuix = document.getElementById("main-cli-guix");
	const cliNg = document.getElementById("main-cli-nonguix");
	if (cliGuix && cliNg) {
		cliGuix.style.display = checked ? "none" : "flex";
		cliNg.style.display = checked ? "flex" : "none";
	}
	document.querySelectorAll(".col-nonguix").forEach((el) => {
		el.style.display = checked ? "" : "none";
	});
	document.querySelectorAll("tbody tr").forEach((tr) => {
		const isLatest = checked
			? tr.getAttribute("data-latest-nonguix") === "1"
			: tr.getAttribute("data-latest-guix") === "1";
		tr.classList.toggle("row-latest-ready", isLatest);
	});
	document.querySelectorAll(".latest-guix-badge").forEach((el) => {
		el.style.display = checked ? "none" : "";
	});
	document.querySelectorAll(".latest-nonguix-badge").forEach((el) => {
		el.style.display = checked ? "" : "none";
	});
	syncNonguixURLParam(checked);
}
