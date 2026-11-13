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
	evalCurrentPage = 1;
	renderUnavailablePackages();
	syncNonguixURLParam(checked);
}

let unavailableData = null;
let evalCurrentPage = 1;
let evalPageSize = 100;
let evalTotalPages = 1;

initUnavailablePackages();

function initUnavailablePackages() {
	const scriptEl = document.getElementById("unavailable-pkgs-data");
	if (!scriptEl) return;
	try {
		unavailableData = JSON.parse(scriptEl.textContent || "{}");
		if (unavailableData) {
			if (unavailableData.guix || unavailableData.nonguix) {
				expandPayloadItems(unavailableData.guix);
				expandPayloadItems(unavailableData.nonguix);
			} else {
				expandPayloadItems(unavailableData);
			}
		}
	} catch (_) {
		unavailableData = null;
		return;
	}
	const validStatuses = ["succeeded", "unavailable", "failed", "failed-build", "failed-dependency", "pending", "stuck", "not-built", "canceled-other"];
	const urlStatus = new URLSearchParams(window.location.search).get("status");
	const statusFilter = document.getElementById("unavailable-status-filter");
	if (urlStatus && statusFilter && validStatuses.indexOf(urlStatus) !== -1) {
		statusFilter.value = urlStatus;
	}
	renderUnavailablePackages();
}

function expandPayloadItems(payload) {
	if (!payload) return;
	const combined = Array.isArray(payload.items) ? payload.items.slice() : [];
	if (Array.isArray(payload.succeeded)) {
		for (let i = 0; i < payload.succeeded.length; i++) {
			combined.push({ name: payload.succeeded[i], channel: "guix", status: "succeeded" });
		}
	}
	if (Array.isArray(payload.nonguix_succeeded)) {
		for (let i = 0; i < payload.nonguix_succeeded.length; i++) {
			combined.push({ name: payload.nonguix_succeeded[i], channel: "nonguix", status: "succeeded" });
		}
	}
	combined.sort((a, b) => {
		if (a.name !== b.name) return a.name < b.name ? -1 : 1;
		return a.channel < b.channel ? -1 : (a.channel > b.channel ? 1 : 0);
	});
	payload.all_items = combined;
}

function getUnavailableActivePayload() {
	if (!unavailableData) return null;
	if (!unavailableData.guix && !unavailableData.nonguix) return unavailableData;
	const ngToggle = document.getElementById("main-nonguix-toggle");
	const useNonguix = ngToggle && ngToggle.checked;
	return useNonguix ? unavailableData.nonguix : unavailableData.guix;
}

function escapeHTML(str) {
	return String(str)
		.replace(/&/g, "&amp;")
		.replace(/</g, "&lt;")
		.replace(/>/g, "&gt;")
		.replace(/"/g, "&quot;")
		.replace(/'/g, "&#39;");
}

function normalizeSpecToken(tok) {
	let s = tok.trim().toLowerCase();
	const colonIdx = s.indexOf(":");
	if (colonIdx > 0) s = s.slice(0, colonIdx);
	const atIdx = s.indexOf("@");
	if (atIdx > 0) s = s.slice(0, atIdx);
	return s;
}

function matchesUnavailableQuery(item, rawQuery) {
	const q = (rawQuery || "").trim().toLowerCase();
	if (!q) return true;
	const tokens = q.split(/[\s,]+/).filter(Boolean);
	if (tokens.length === 0) return true;

	const itemName = (item && item.name) ? item.name : String(item || "");
	const lowerName = itemName.toLowerCase();
	const atIdx = lowerName.indexOf("@");
	const baseName = atIdx > 0 ? lowerName.slice(0, atIdx) : lowerName;

	const candidateNames = [lowerName];
	if (item && Array.isArray(item.builds)) {
		for (let i = 0; i < item.builds.length; i++) {
			const b = item.builds[i];
			if (b && b.version) {
				candidateNames.push(baseName + "@" + String(b.version).toLowerCase());
			}
		}
	}

	if (tokens.length === 1) {
		const tok = tokens[0];
		const baseTok = normalizeSpecToken(tok);
		for (let i = 0; i < candidateNames.length; i++) {
			const cand = candidateNames[i];
			if (cand.includes(tok) || (baseTok && cand.includes(baseTok))) {
				return true;
			}
		}
		return false;
	}

	for (let i = 0; i < tokens.length; i++) {
		const tok = tokens[i];
		const baseTok = normalizeSpecToken(tok);
		if (lowerName === tok || baseName === tok || (baseTok && (lowerName === baseTok || baseName === baseTok))) {
			return true;
		}
		for (let j = 0; j < candidateNames.length; j++) {
			const cand = candidateNames[j];
			if (cand === tok || cand.startsWith(tok)) {
				return true;
			}
		}
	}
	return false;
}

function matchesUnavailableStatus(status, filterVal) {
	if (!filterVal) return true;
	if (filterVal === "succeeded") {
		return status === "succeeded";
	}
	if (filterVal === "unavailable") {
		return status !== "succeeded";
	}
	if (filterVal === "failed") {
		return status === "failed" || status === "failed-dependency" || status === "failed-other" || status === "canceled";
	}
	if (filterVal === "failed-build") {
		return status === "failed";
	}
	if (filterVal === "pending") {
		return status === "scheduled" || status === "submitted" || status === "started";
	}
	if (filterVal === "canceled-other") {
		return status === "canceled" || status === "failed-other";
	}
	return status === filterVal;
}

function matchesItemStatus(item, filterVal) {
	if (!filterVal) return true;
	if (item && Array.isArray(item.builds) && item.builds.length > 0) {
		for (let i = 0; i < item.builds.length; i++) {
			if (matchesUnavailableStatus(item.builds[i].status, filterVal)) {
				return true;
			}
		}
		return false;
	}
	return matchesUnavailableStatus(item ? item.status : "", filterVal);
}

function formatStatusBadge(status) {
	switch (status) {
		case "succeeded":
			return '<span class="badge badge-ok">Succeeded</span>';
		case "failed":
			return '<span class="badge badge-danger">Failed</span>';
		case "failed-dependency":
			return '<span class="badge badge-warn">Failed (dependency)</span>';
		case "failed-other":
			return '<span class="badge badge-warn">Failed (other)</span>';
		case "scheduled":
		case "submitted":
		case "started":
			return '<span class="badge badge-warn">Pending</span>';
		case "stuck":
			return '<span class="badge badge-warn">Stuck</span>';
		case "canceled":
			return '<span class="badge badge-muted">Canceled</span>';
		case "not-built":
			return '<span class="badge badge-muted">Not built</span>';
		default:
			return '<span class="badge badge-warn">' + escapeHTML(status) + '</span>';
	}
}

function buildCuirassInspectLink(payload, item) {
	const isNg = item.channel === "nonguix";
	const serverURL = isNg
		? (payload.nonguix_url || "https://cuirass.nonguix.org")
		: (payload.server_url || "https://ci.guix.gnu.org");
	if (item.build_id && item.build_id > 0) {
		return {
			url: serverURL + "/build/" + item.build_id + "/details",
			label: "Build #" + item.build_id + " &rarr;"
		};
	}
	const atIdx = item.name.indexOf("@");
	const basePkg = atIdx > 0 ? item.name.slice(0, atIdx) : item.name;
	const spec = isNg ? "nonguix" : (payload.spec || "master");
	const query = "spec:" + spec + " system:" + payload.system + " " + basePkg;
	return {
		url: serverURL + "/search?query=" + encodeURIComponent(query),
		label: "Cuirass builds &rarr;"
	};
}

function setUnavailableStatusFilter(ev, status) {
	if (ev && (ev.metaKey || ev.ctrlKey || ev.shiftKey || ev.altKey || (typeof ev.button === "number" && ev.button !== 0))) {
		return true;
	}
	const statusFilter = document.getElementById("unavailable-status-filter");
	if (!statusFilter) return true;
	statusFilter.value = status || "";
	filterUnavailablePackages();
	const panel = document.getElementById("unavailable-panel");
	if (panel && panel.scrollIntoView) {
		panel.scrollIntoView({ behavior: "smooth", block: "nearest" });
	}
	return false;
}

function filterUnavailablePackages() {
	evalCurrentPage = 1;
	if (window.history && window.history.replaceState) {
		const statusFilter = document.getElementById("unavailable-status-filter");
		const url = new URL(window.location.href);
		if (statusFilter && statusFilter.value) {
			url.searchParams.set("status", statusFilter.value);
		} else {
			url.searchParams.delete("status");
		}
		window.history.replaceState(null, "", url.pathname + url.search + url.hash);
	}
	renderUnavailablePackages(true);
}

function goToEvalPage(rawPage) {
	const parsed = parseInt(rawPage, 10);
	if (isNaN(parsed)) {
		renderUnavailablePackages(false);
		return;
	}
	evalCurrentPage = Math.min(Math.max(1, parsed), evalTotalPages);
	renderUnavailablePackages(true);
}

function stepEvalPage(delta) {
	goToEvalPage(evalCurrentPage + delta);
}

function goToEvalLastPage() {
	goToEvalPage(evalTotalPages);
}

function changeEvalPageSize(rawSize) {
	const parsed = parseInt(rawSize, 10);
	if (!isNaN(parsed) && parsed > 0) {
		evalPageSize = parsed;
	}
	evalCurrentPage = 1;
	renderUnavailablePackages(true);
}

function renderUnavailablePackages(resetScroll) {
	const payload = getUnavailableActivePayload();
	const availBadge = document.getElementById("eval-available-badge");
	const totalBadge = document.getElementById("unavailable-total-badge");
	const evalMeta = document.getElementById("unavailable-eval-meta");
	const matchCountEl = document.getElementById("unavailable-match-count");
	const tbody = document.getElementById("unavailable-tbody");
	const tableWrap = document.getElementById("eval-table-wrap");
	const paginationWrap = document.getElementById("eval-pagination-wrap");
	const pageRangeEl = document.getElementById("eval-page-range");
	const pageInputEl = document.getElementById("eval-page-input");
	const pageTotalEl = document.getElementById("eval-page-total");
	const firstBtn = document.getElementById("eval-page-first");
	const prevBtn = document.getElementById("eval-page-prev");
	const nextBtn = document.getElementById("eval-page-next");
	const lastBtn = document.getElementById("eval-page-last");
	if (!tbody || !payload) return;

	const items = Array.isArray(payload.all_items) ? payload.all_items : (Array.isArray(payload.items) ? payload.items : []);
	const availCount = typeof payload.available_count === "number" ? payload.available_count : 0;
	const unavailCount = typeof payload.unavailable_count === "number" ? payload.unavailable_count : items.length;
	const totalCount = availCount + unavailCount;

	if (availBadge) {
		availBadge.className = "badge badge-ok";
		availBadge.textContent = availCount + " available";
	}
	if (totalBadge) {
		if (!payload.available) {
			totalBadge.className = "badge badge-muted";
			totalBadge.textContent = "No ready evaluation";
		} else if (unavailCount === 0) {
			totalBadge.className = "badge badge-ok";
			totalBadge.textContent = "0 unavailable";
		} else {
			totalBadge.className = "badge badge-danger";
			totalBadge.textContent = unavailCount + " unavailable";
		}
	}

	if (evalMeta) {
		if (!payload.available) {
			evalMeta.textContent = "";
		} else if (payload.evaluation_id > 0) {
			const prefix = payload.spec === "guix" ? "guix/guix #" : "guix #";
			let metaText = prefix + payload.evaluation_id + " (" + (payload.commit || "").slice(0, 10) + ")";
			if (payload.nonguix_eval_id > 0) {
				metaText += " + nonguix #" + payload.nonguix_eval_id + " (" + (payload.nonguix_commit || "").slice(0, 10) + ")";
			}
			evalMeta.textContent = metaText;
		} else if (payload.nonguix_eval_id > 0) {
			evalMeta.textContent = "nonguix #" + payload.nonguix_eval_id + " (" + (payload.nonguix_commit || "").slice(0, 10) + ")";
		}
	}

	if (!payload.available) {
		if (matchCountEl) matchCountEl.textContent = "";
		tbody.innerHTML = '<tr><td colspan="4" style="color:var(--muted);text-align:center;padding:1rem;">No evaluation available for the selected configuration.</td></tr>';
		if (paginationWrap) paginationWrap.style.display = "none";
		return;
	}

	const searchInput = document.getElementById("unavailable-search");
	const statusFilter = document.getElementById("unavailable-status-filter");
	const rawQuery = searchInput ? searchInput.value : "";
	const statusVal = statusFilter ? statusFilter.value : "";

	const filtered = items.filter((item) => {
		return matchesItemStatus(item, statusVal) && matchesUnavailableQuery(item, rawQuery);
	});

	if (matchCountEl) {
		if (rawQuery.trim() || statusVal) {
			matchCountEl.textContent = "Matching " + filtered.length + " of " + totalCount + " packages (" + availCount + " available, " + unavailCount + " unavailable)";
		} else {
			matchCountEl.textContent = "Total: " + totalCount + " packages (" + availCount + " available, " + unavailCount + " unavailable)";
		}
	}

	if (filtered.length === 0) {
		const emptyMsg = items.length === 0
			? "No packages tracked in this evaluation."
			: "No packages match your filter.";
		tbody.innerHTML = '<tr><td colspan="4" style="color:var(--muted);text-align:center;padding:1rem;">' + escapeHTML(emptyMsg) + '</td></tr>';
		if (paginationWrap) paginationWrap.style.display = "none";
		return;
	}

	evalTotalPages = Math.max(1, Math.ceil(filtered.length / evalPageSize));
	if (evalCurrentPage > evalTotalPages) evalCurrentPage = evalTotalPages;
	if (evalCurrentPage < 1) evalCurrentPage = 1;

	const startIdx = (evalCurrentPage - 1) * evalPageSize;
	const endIdx = Math.min(startIdx + evalPageSize, filtered.length);

	const ngToggle = document.getElementById("main-nonguix-toggle");
	const useNonguix = Boolean(ngToggle && ngToggle.checked);

	const visible = filtered.slice(startIdx, endIdx);
	const rowsHTML = visible.map((item) => {
		const ch = item.channel || "guix";
		const params = [];
		if (payload.system && payload.system !== "x86_64-linux") {
			params.push("system=" + encodeURIComponent(payload.system));
		}
		if (useNonguix || ch === "nonguix") {
			params.push("additional_channels=nonguix");
		}
		const querySuffix = params.length > 0 ? "?" + params.join("&") : "";
		const pkgURL = "/packages/" + encodeURIComponent(item.name).replace(/%40/g, "@") + querySuffix;
		const escName = escapeHTML(item.name);
		const escCh = escapeHTML(ch);
		const escPkgURL = escapeHTML(pkgURL);

		if (Array.isArray(item.builds) && item.builds.length > 1) {
			const pkgCell = '<a href="' + escPkgURL + '">' + escName + '</a> <span class="badge badge-muted" style="margin-left:0.35rem;">' + item.builds.length + ' builds</span>';
			const statusParts = [];
			const inspectParts = [];
			for (let i = 0; i < item.builds.length; i++) {
				const b = item.builds[i];
				let verPrefix = "";
				if (b.version) {
					verPrefix = '<span class="mono" style="font-size:0.78rem;color:var(--muted);">@' + escapeHTML(b.version) + '</span> ';
				} else if (b.build_id > 0) {
					verPrefix = '<span class="mono" style="font-size:0.78rem;color:var(--muted);">#' + b.build_id + '</span> ';
				}
				statusParts.push('<div style="display:inline-flex;align-items:center;gap:0.35rem;">' + verPrefix + formatStatusBadge(b.status) + '</div>');
				const bInspect = buildCuirassInspectLink(payload, {
					name: item.name,
					channel: ch,
					status: b.status,
					build_id: b.build_id
				});
				inspectParts.push('<div><a href="' + escapeHTML(bInspect.url) + '" target="_blank" rel="noopener" class="mono" style="font-size:0.8rem;">' + bInspect.label + '</a></div>');
			}
			return '<tr>' +
				'<td class="mono">' + pkgCell + '</td>' +
				'<td class="mono"><span class="badge badge-muted">' + escCh + '</span></td>' +
				'<td><div style="display:flex;flex-direction:column;gap:0.28rem;">' + statusParts.join("") + '</div></td>' +
				'<td><div style="display:flex;flex-direction:column;gap:0.28rem;">' + inspectParts.join("") + '</div></td>' +
				'</tr>';
		}

		const inspect = buildCuirassInspectLink(payload, item);
		const escURL = escapeHTML(inspect.url);
		let statusCell = formatStatusBadge(item.status);
		if (Array.isArray(item.builds) && item.builds.length === 1 && item.builds[0].version) {
			statusCell = '<div style="display:inline-flex;align-items:center;gap:0.35rem;"><span class="mono" style="font-size:0.78rem;color:var(--muted);">@' + escapeHTML(item.builds[0].version) + '</span>' + statusCell + '</div>';
		}
		return '<tr>' +
			'<td class="mono"><a href="' + escPkgURL + '">' + escName + '</a></td>' +
			'<td class="mono"><span class="badge badge-muted">' + escCh + '</span></td>' +
			'<td>' + statusCell + '</td>' +
			'<td><a href="' + escURL + '" target="_blank" rel="noopener" class="mono" style="font-size:0.8rem;">' + inspect.label + '</a></td>' +
			'</tr>';
	}).join("");
	tbody.innerHTML = rowsHTML;
	if (resetScroll && tableWrap) {
		tableWrap.scrollTop = 0;
	}

	if (paginationWrap) {
		paginationWrap.style.display = "";
	}
	if (pageRangeEl) {
		pageRangeEl.textContent = "Showing " + (startIdx + 1) + "-" + endIdx + " of " + filtered.length;
	}
	if (pageInputEl) {
		pageInputEl.value = String(evalCurrentPage);
		pageInputEl.max = String(evalTotalPages);
	}
	if (pageTotalEl) {
		pageTotalEl.textContent = String(evalTotalPages);
	}
	const isFirst = evalCurrentPage <= 1;
	const isLast = evalCurrentPage >= evalTotalPages;
	if (firstBtn) firstBtn.disabled = isFirst;
	if (prevBtn) prevBtn.disabled = isFirst;
	if (nextBtn) nextBtn.disabled = isLast;
	if (lastBtn) lastBtn.disabled = isLast;
}

function togglePackageNonguix(checked) {
	const url = new URL(window.location.href);
	if (checked) {
		url.searchParams.set("additional_channels", "nonguix");
	} else {
		url.searchParams.delete("additional_channels");
	}
	window.location.href = url.pathname + url.search + url.hash;
}

function jumpToPackage(formEl) {
	if (!formEl) return false;
	const input = formEl.querySelector('input[name="pkg"]');
	if (!input) return false;
	const pkg = (input.value || "").trim();
	if (!pkg) return false;
	const url = new URL(window.location.href);
	url.searchParams.delete("status");
	window.location.href = "/packages/" + encodeURIComponent(pkg) + url.search;
	return false;
}
