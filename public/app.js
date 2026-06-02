// ─── Sidebar collapse persistence ──────────────────────────────────────────
$(document).on('collapsed.pushMenu', function () {
    localStorage.setItem('ff3ai_sidebar_collapsed', 'true');
});
$(document).on('expanded.pushMenu', function () {
    localStorage.setItem('ff3ai_sidebar_collapsed', 'false');
});

// ─── State ─────────────────────────────────────────────────────────────────
var jobs = {};
var txnPage = 1, txnTotalPages = 1, txnTotal = 0;
var selectedTxns = new Set();
var cachedCategories = null; // {html, data} — rendered cache for instant display
var cachedAccounts = null;   // {html, data} — rendered cache for instant display
var reviewPollTimer = null;  // interval ID for background review polling

// ─── Tab navigation ─────────────────────────────────────────────────────────
var TAB_META = {
    jobs: {title: 'Jobs', sub: 'Recent classification activity'},
    transactions: {title: 'Transactions', sub: 'Select and re-categorize transactions'},
    review: {title: 'Review', sub: 'Review categories, destinations, and conversions'},
    categories: {title: 'Categories', sub: 'Categories available to the AI'},
    accounts: {title: 'Accounts', sub: 'Expense accounts for destination matching'},
    settings: {title: 'Settings', sub: 'Configure AI provider and connection'},
};

function switchTab(name) {
    Object.keys(TAB_META).forEach(function (t) {
        document.getElementById('tab-' + t).style.display = (t === name) ? '' : 'none';
        document.getElementById('nav-' + t).classList.toggle('active', t === name);
    });
    var m = TAB_META[name];
    document.getElementById('page-title').innerHTML = esc(m.title) + ' <small>' + esc(m.sub) + '</small>';
    document.getElementById('breadcrumb-leaf').textContent = m.title;
    if (name === 'settings') loadSettings();
    if (name === 'transactions') populateTxnCatFilter();
    if (name === 'categories') {
        if (cachedCategories) { renderCachedCategories(); }
        loadCategories();
    }
    if (name === 'accounts') {
        if (cachedAccounts) { renderCachedAccounts(); }
        loadAccounts();
    }
    // Manage review auto-polling.
    if (reviewPollTimer) { clearInterval(reviewPollTimer); reviewPollTimer = null; }
    if (name === 'review') {
        // Show destination and transfer sections immediately when applicable,
        // before the async loadReview completes.
        document.getElementById('review-section-destination').style.display =
            savedConfig.destination_match_enabled ? '' : 'none';
        loadReview();
        // Poll every 30 seconds for new items to review.
        reviewPollTimer = setInterval(function () {
            if (document.getElementById('tab-review').style.display === 'none') {
                clearInterval(reviewPollTimer);
                reviewPollTimer = null;
                return;
            }
            loadReview(true);
        }, 30000);
    }
    return false;
}

// ─── SSE ───────────────────────────────────────────────────────────────────
function setConn(state) {
    var c = {live: '#00a65a', error: '#dd4b39', connecting: '#d2d6de'};
    var l = {live: 'Live', error: 'Disconnected', connecting: 'Connecting…'};
    $('#conn-dot').css('color', c[state]);
    $('#conn-label').text(l[state]);
}

function connectSSE() {
    var es = new EventSource('/events');
    es.onopen = function () {setConn('live');};
    es.onerror = function () {setConn('error'); setTimeout(connectSSE, 3000);};
    es.onmessage = function (e) {
        var data = JSON.parse(e.data);
        if (data.type === 'snapshot') {
            (data.jobs || []).forEach(function (j) {jobs[j.id] = j;});
            renderJobTable();
        } else if (data.type === 'created') {
            jobs[data.job.id] = data.job;
            prependJobRow(data.job);
            updateStats();
        } else if (data.type === 'updated') {
            jobs[data.job.id] = data.job;
            var row = document.querySelector('tr[data-job="' + data.job.id + '"]');
            if (row) row.replaceWith(buildJobRow(data.job));
            var det = document.getElementById('detail-' + data.job.id);
            if (det) $(det).find('.detail-inner').html(buildDetailInner(data.job));
            updateStats();
        }
    };
}
connectSSE();

// ─── Theme (dark mode) ─────────────────────────────────────────────────────
var _schemeListener = null;

function applyDark(dark) {
    document.body.classList.toggle('dark-mode', dark);
    var existing = document.getElementById('dark-theme-css');
    if (dark && !existing) {
        var link = document.createElement('link');
        link.id = 'dark-theme-css';
        link.rel = 'stylesheet';
        link.href = '/darkmode.css';
        document.head.appendChild(link);
    } else if (!dark && existing) {
        existing.remove();
    }
}

async function applyTheme() {
    // Remove any previous prefers-color-scheme listener before re-fetching.
    var mq = window.matchMedia('(prefers-color-scheme: dark)');
    if (_schemeListener) {
        mq.removeEventListener('change', _schemeListener);
        _schemeListener = null;
    }
    try {
        var d = await (await fetch('/api/theme')).json();
        if (d.browser) {
            localStorage.setItem('ff3ai_theme', 'browser');
            applyDark(mq.matches);
            _schemeListener = function (e) {applyDark(e.matches);};
            mq.addEventListener('change', _schemeListener);
        } else {
            localStorage.setItem('ff3ai_theme', d.dark ? 'dark' : 'light');
            applyDark(!!d.dark);
        }
    } catch (e) { }
}
applyTheme();

// ─── Initial config check ──────────────────────────────────────────────────
// Navigate to Settings automatically on first load if credentials are missing.
// Also populates savedConfig so other pages (Review, Categories) can access
// feature flags without needing to visit Settings first.
(async function () {
    try {
        var d = await (await fetch('/api/config')).json();
        savedConfig = d;
        if (!d.configured) switchTab('settings');
    } catch (e) { }
})();

// ─── Jobs ──────────────────────────────────────────────────────────────────
function renderJobTable() {
    var tbody = document.getElementById('job-tbody');
    tbody.innerHTML = '';
    var sorted = Object.values(jobs).sort(function (a, b) {
        return new Date(b.created_at) - new Date(a.created_at);
    });
    if (!sorted.length) {
        tbody.innerHTML = '<tr id="job-empty-row"><td colspan="7" class="text-center text-muted" style="padding:40px 0">'
            + '<i class="fa fa-inbox fa-3x" style="display:block;margin-bottom:10px;opacity:.3"></i>'
            + 'No jobs yet &mdash; waiting for webhooks or run a batch.</td></tr>';
    } else {
        sorted.forEach(function (j) {tbody.appendChild(buildJobRow(j));});
    }
    updateStats();
}

function prependJobRow(j) {
    var tbody = document.getElementById('job-tbody');
    var empty = document.getElementById('job-empty-row');
    if (empty) empty.remove();
    tbody.insertBefore(buildJobRow(j), tbody.firstChild);
}

function buildJobRow(j) {
    var tr = document.createElement('tr');
    tr.setAttribute('data-job', j.id);
    tr.onclick = function () {toggleDetail(j.id);};

    var lbl = jobLabel(j);
    var amount = j.amount != null ? parseFloat(j.amount).toFixed(2) : '&mdash;';
    var t = new Intl.DateTimeFormat(undefined, {dateStyle: 'short', timeStyle: 'short'}).format(new Date(j.created_at));

    // Show AI-assigned destination account when present, otherwise the original payee name.
    var dest = j.destination_account || j.destination_name || '&mdash;';
    var destHtml = '<strong>' + esc(dest) + '</strong>';
    if (j.destination_account && j.destination_action === 'MATCH') {
        destHtml += ' <i class="fa fa-link text-muted" title="Matched to existing account" style="font-size:11px"></i>';
    } else if (j.destination_account && j.destination_action === 'CREATE') {
        destHtml += ' <i class="fa fa-plus-circle text-success" title="Account created" style="font-size:11px"></i>';
    }

    tr.innerHTML =
        '<td style="width:18px;vertical-align:middle"><i class="fa fa-chevron-right text-muted" id="ic-' + j.id + '" style="font-size:10px"></i></td>'
        + '<td>' + destHtml + '</td>'
        + '<td class="text-muted hidden-xs">' + esc(trunc(j.description, 55)) + '</td>'
        + '<td class="text-right hidden-sm hidden-xs">' + amount + '</td>'
        + '<td>' + (j.category ? '<span class="label label-default">' + esc(j.category) + '</span>' : '<span class="text-muted">&mdash;</span>') + '</td>'
        + '<td><span class="label ' + lbl.cls + '">' + lbl.txt + '</span></td>'
        + '<td class="text-right hidden-xs text-muted" style="font-size:12px;white-space:nowrap">' + t + '</td>';
    return tr;
}

function toggleDetail(id) {
    var existing = document.getElementById('detail-' + id);
    var icon = document.getElementById('ic-' + id);
    if (existing) {
        existing.remove();
        if (icon) {icon.classList.remove('fa-chevron-down'); icon.classList.add('fa-chevron-right');}
        return;
    }
    var j = jobs[id];
    if (!j) return;
    var mainRow = document.querySelector('tr[data-job="' + id + '"]');
    if (!mainRow) return;
    if (icon) {icon.classList.remove('fa-chevron-right'); icon.classList.add('fa-chevron-down');}
    var tr = document.createElement('tr');
    tr.id = 'detail-' + id;
    tr.className = 'job-detail-row';
    tr.onclick = function (e) {e.stopPropagation();};
    tr.innerHTML = '<td colspan="7"><div class="detail-inner">' + buildDetailInner(j) + '</div></td>';
    mainRow.after(tr);
}

function buildDetailInner(j) {
    var html = '';
    if (j.destination_account) {
        var dLabel = j.destination_action === 'CREATE'
            ? '<i class="fa fa-plus-circle text-success"></i> Created account'
            : '<i class="fa fa-link"></i> Matched account';
        html += '<p><strong>Destination:</strong> ' + dLabel + ' <strong>' + esc(j.destination_account) + '</strong></p>';
    }
    if (j.reason) html += '<p><strong>Reason:</strong> ' + esc(j.reason) + '</p>';
    if (j.assumption) html += '<p><strong>Assumption:</strong> <em class="text-warning">' + esc(j.assumption) + '</em></p>';
    if (j.error) {
        html += '<div style="display:flex;align-items:flex-start;gap:8px;margin:0 0 8px">'
            + '<div class="alert alert-danger" style="margin:0;flex:1"><i class="fa fa-warning"></i> ' + esc(j.error) + '</div>'
            + '<button class="btn btn-default btn-sm" style="white-space:nowrap;flex-shrink:0"'
            + ' onclick="retryJob(\'' + j.id + '\',\'' + j.transaction_id + '\',this)">'
            + '<i class="fa fa-refresh"></i> Retry</button>'
            + '</div>';
    }
    if (j.raw_prompt || j.raw_response) {
        html += '<div class="row">';
        if (j.raw_prompt) html += '<div class="col-md-6"><p class="text-muted" style="margin-bottom:4px;font-size:12px"><strong>Prompt</strong></p><pre class="detail-pre">' + esc(j.raw_prompt) + '</pre></div>';
        if (j.raw_response) html += '<div class="col-md-6"><p class="text-muted" style="margin-bottom:4px;font-size:12px"><strong>Response</strong></p><pre class="detail-pre">' + esc(j.raw_response) + '</pre></div>';
        html += '</div>';
    }
    if (!html) html = '<span class="text-muted">No details available yet.</span>';
    return html;
}

function jobLabel(j) {
    if (j.status === 'queued') return {cls: 'label-default', txt: 'Queued'};
    if (j.status === 'in_progress') return {cls: 'label-info', txt: 'Running'};
    if (j.status === 'failed') return {cls: 'label-danger', txt: 'Failed'};
    if (j.outcome === 'CLASSIFIED') return {cls: 'label-success', txt: 'Classified'};
    if (j.outcome === 'ASSUMED') return {cls: 'label-warning', txt: 'Assumed'};
    if (j.outcome === 'NEEDS_REVIEW') return {cls: 'label-danger', txt: 'Needs Review'};
    return {cls: 'label-default', txt: j.status};
}

function updateStats() {
    var vals = Object.values(jobs);
    $('#stat-total').text(vals.length);
    $('#stat-running').text(vals.filter(function (j) {return j.status === 'in_progress';}).length);
    $('#stat-classified').text(vals.filter(function (j) {return j.outcome === 'CLASSIFIED';}).length);
    $('#stat-assumed').text(vals.filter(function (j) {return j.outcome === 'ASSUMED';}).length);
    $('#stat-review').text(vals.filter(function (j) {return j.outcome === 'NEEDS_REVIEW';}).length);
    $('#stat-failed').text(vals.filter(function (j) {return j.status === 'failed';}).length);
    var running = vals.filter(function (j) {return j.status === 'in_progress';}).length;
    $('#jobs-running-badge').toggle(running > 0).text(running);
}

// ─── Retry failed job ──────────────────────────────────────────────────────
async function retryJob(jobId, transactionId, btn) {
    btn.disabled = true;
    btn.innerHTML = '<i class="fa fa-spinner fa-spin"></i> Retrying…';
    try {
        var res = await fetch('/batch/run', {
            method: 'POST',
            headers: {'Content-Type': 'application/json'},
            body: JSON.stringify({filter: {transaction_ids: [transactionId]}, force: true})
        });
        if (res.status === 503) throw new Error('Not configured — check Settings.');
        if (!res.ok) throw new Error(await res.text());
        // New job will appear at the top via SSE; collapse the failed job's detail row.
        toggleDetail(jobId);
    } catch (e) {
        btn.disabled = false;
        btn.innerHTML = '<i class="fa fa-refresh"></i> Retry';
        var alertEl = btn.previousElementSibling;
        if (alertEl) alertEl.innerHTML = '<i class="fa fa-warning"></i> Retry failed: ' + esc(e.message);
    }
}

// ─── Transactions ──────────────────────────────────────────────────────────
function txnFlash(msg, type) {
    var el = document.getElementById('txn-flash');
    if (!msg) {el.innerHTML = ''; return;}
    el.innerHTML = '<div class="row"><div class="col-md-12"><div class="alert alert-' + type + '">' + msg + '</div></div></div>';
}

// Populate the category filter dropdown from cached categories or fetch fresh.
async function populateTxnCatFilter() {
    var sel = document.getElementById('txn-filter-category');
    var currentVal = sel.value;
    try {
        var res = await fetch('/api/categories');
        if (!res.ok) return;
        var cats = await res.json();
        sel.innerHTML = '<option value="">— all categories —</option>';
        cats.forEach(function (c) {
            sel.innerHTML += '<option value="' + esc(c.name) + '">' + esc(c.name) + '</option>';
        });
        sel.value = currentVal;
    } catch (e) {}
}

// Prevent conflicting destination filters: when "Missing destination" is
// checked, disable the destination text input and clear it.
function onMissingDestChange() {
    var checked = $('#txn-filter-missing-dest').prop('checked');
    var input = $('#txn-filter-dest');
    if (checked) {
        input.val('').prop('disabled', true);
    } else {
        input.prop('disabled', false);
    }
}

// When "Missing category" is checked, disable the category select.
function onMissingCatChange() {
    var checked = $('#txn-filter-missing-cat').prop('checked');
    var sel = $('#txn-filter-category');
    if (checked) {
        sel.val('').prop('disabled', true);
    } else {
        sel.prop('disabled', false);
    }
}

function clearTxnFilters() {
    $('#txn-filter-category').val('').prop('disabled', false);
    $('#txn-filter-dest').val('').prop('disabled', false);
    $('#txn-filter-missing-cat').prop('checked', false);
    $('#txn-filter-missing-dest').prop('checked', false);
    loadTransactions();
}

async function loadTransactions(page) {
    if (!page) page = 1;
    txnPage = page;
    var start = $('#txn-start').val(), end = $('#txn-end').val();
    var params = new URLSearchParams({page: page, limit: 50});
    if (start) params.set('start', start);
    if (end) params.set('end', end);
    if ($('#txn-filter-missing-cat').prop('checked')) params.set('missing_category', 'true');
    if ($('#txn-filter-missing-dest').prop('checked')) params.set('missing_destination', 'true');
    var destFilter = $('#txn-filter-dest').val().trim();
    if (destFilter) params.set('destination', destFilter);
    var catFilter = $('#txn-filter-category').val();
    if (catFilter) params.set('category', catFilter);
    txnFlash('', '');
    $('#txn-select-all-bar').hide();
    $('#txn-select-all').prop('checked', false);
    $('#txn-tbody').html('<tr><td colspan="7" class="text-center" style="padding:30px">'
        + '<i class="fa fa-spinner fa-spin"></i> Loading&hellip;</td></tr>');
    try {
        var res = await fetch('/api/transactions?' + params);
        if (res.status === 503) {
            txnFlash('<i class="fa fa-warning"></i> Firefly III is not configured. Set credentials in Settings.', 'warning');
            $('#txn-tbody').html(''); return;
        }
        if (!res.ok) throw new Error(await res.text());
        var d = await res.json();
        txnTotalPages = d.total_pages || 1;
        txnTotal = d.total || 0;
        renderTxnTable(d.data || []);
        renderTxnPagination();
    } catch (e) {
        txnFlash('<i class="fa fa-times-circle"></i> Failed to load transactions: ' + esc(e.message), 'danger');
        $('#txn-tbody').html('');
    }
}

function renderTxnTable(rows) {
    if (!rows.length) {
        $('#txn-tbody').html('<tr><td colspan="7" class="text-center text-muted" style="padding:40px 0">No transactions found for the selected period.</td></tr>');
        $('#txn-footer').hide(); return;
    }
    $('#txn-footer').show();
    var html = rows.map(function (r) {
        var checked = selectedTxns.has(r.id) ? 'checked' : '';
        var cls = selectedTxns.has(r.id) ? ' class="selected"' : '';
        var date = r.date ? r.date.substring(0, 10) : '';
        var aiTag = aiTagLabel(r.tags);
        return '<tr' + cls + '>'
            + '<td><input type="checkbox" data-id="' + r.id + '" ' + checked + ' onchange="toggleTxn(this,\'' + r.id + '\')"></td>'
            + '<td style="white-space:nowrap">' + esc(date) + '</td>'
            + '<td><strong>' + esc(trunc(r.destination_name, 32)) + '</strong></td>'
            + '<td class="text-muted hidden-xs">' + esc(trunc(r.description, 42)) + '</td>'
            + '<td class="text-right">' + (isNaN(parseFloat(r.amount)) ? '&mdash;' : parseFloat(r.amount).toFixed(2)) + '</td>'
            + '<td>' + (r.category_name ? '<span class="label label-default">' + esc(r.category_name) + '</span>' : '<span class="text-muted">&mdash;</span>') + '</td>'
            + '<td class="hidden-xs">' + aiTag + '</td>'
            + '</tr>';
    }).join('');
    $('#txn-tbody').html(html);
}

function aiTagLabel(tags) {
    if (!tags || !tags.length) return '';
    for (var i = 0; i < tags.length; i++) {
        if (tags[i].indexOf(':classified') >= 0) return '<span class="label label-success">classified</span>';
        if (tags[i].indexOf(':assumed') >= 0) return '<span class="label label-warning">assumed</span>';
        if (tags[i].indexOf(':needs-review') >= 0) return '<span class="label label-danger">needs review</span>';
        if (tags[i].indexOf(':reviewed') >= 0) return '<span class="label label-info">reviewed</span>';
    }
    return '';
}

function renderTxnPagination() {
    $('#txn-count').text(txnTotal + ' transaction(s)');
    var lo = Math.max(1, txnPage - 2), hi = Math.min(txnTotalPages, txnPage + 2);
    var html = '<li' + (txnPage <= 1 ? ' class="disabled"' : '') + '>'
        + '<a href="#" onclick="loadTransactions(' + (txnPage - 1) + ');return false">&laquo;</a></li>';
    for (var p = lo; p <= hi; p++) {
        html += '<li' + (p === txnPage ? ' class="active"' : '') + '>'
            + '<a href="#" onclick="loadTransactions(' + p + ');return false">' + p + '</a></li>';
    }
    html += '<li' + (txnPage >= txnTotalPages ? ' class="disabled"' : '') + '>'
        + '<a href="#" onclick="loadTransactions(' + (txnPage + 1) + ');return false">&raquo;</a></li>';
    $('#txn-pagination').html(html);
}

function toggleTxn(cb, id) {
    if (cb.checked) {selectedTxns.add(id); $(cb).closest('tr').addClass('selected');}
    else {selectedTxns.delete(id); $(cb).closest('tr').removeClass('selected');}
    updateTxnSel();
}

function toggleSelectAll(master) {
    var pageIds = [];
    $('#txn-tbody input[type=checkbox]').each(function () {
        var id = $(this).attr('data-id');
        if (!id) return;
        this.checked = master.checked;
        if (master.checked) {selectedTxns.add(id); $(this).closest('tr').addClass('selected'); pageIds.push(id);}
        else {selectedTxns.delete(id); $(this).closest('tr').removeClass('selected');}
    });
    // Reset bar to the initial "select all" prompt (not the post-selectAllPages state).
    if (master.checked && txnTotalPages > 1) {
        $('#txn-page-count').text(pageIds.length);
        $('#txn-all-count').text(txnTotal);
        $('#txn-select-all-bar').html(
            'All <strong id="txn-page-count">' + pageIds.length + '</strong> transactions on this page are selected.'
            + ' &nbsp;<a href="#" onclick="selectAllPages();return false">Select all <strong id="txn-all-count">'
            + txnTotal + '</strong> transactions in this period</a>'
            + ' &nbsp;&mdash;&nbsp;<a href="#" onclick="clearSelection();return false">Clear selection</a>'
        ).show();
    } else {
        $('#txn-select-all-bar').hide();
    }
    updateTxnSel();
}

async function selectAllPages() {
    var start = $('#txn-start').val(), end = $('#txn-end').val();
    var params = new URLSearchParams({ids_only: 'true'});
    if (start) params.set('start', start);
    if (end) params.set('end', end);
    if ($('#txn-filter-missing-cat').prop('checked')) params.set('missing_category', 'true');
    if ($('#txn-filter-missing-dest').prop('checked')) params.set('missing_destination', 'true');
    var destFilter = $('#txn-filter-dest').val().trim();
    if (destFilter) params.set('destination', destFilter);
    var catFilter = $('#txn-filter-category').val();
    if (catFilter) params.set('category', catFilter);
    $('#txn-select-all-bar').html('<i class="fa fa-spinner fa-spin"></i> Fetching all transaction IDs&hellip;');
    try {
        var res = await fetch('/api/transactions?' + params);
        if (!res.ok) throw new Error(await res.text());
        var ids = await res.json();
        ids.forEach(function (id) {selectedTxns.add(id);});
        // Re-render current page checkboxes to reflect full selection
        $('#txn-tbody input[type=checkbox]').each(function () {
            var id = $(this).attr('data-id');
            if (id && selectedTxns.has(id)) {this.checked = true; $(this).closest('tr').addClass('selected');}
        });
        $('#txn-select-all-bar').html(
            'All <strong>' + selectedTxns.size + '</strong> transactions are selected. '
            + '&nbsp;<a href="#" onclick="clearSelection();return false">Clear selection</a>'
        ).show();
        updateTxnSel();
    } catch (e) {
        $('#txn-select-all-bar').html('<span class="text-danger"><i class="fa fa-times"></i> ' + esc(e.message) + '</span>').show();
    }
}

function clearSelection() {
    selectedTxns.clear();
    $('#txn-tbody input[type=checkbox]').prop('checked', false).closest('tr').removeClass('selected');
    $('#txn-select-all').prop('checked', false);
    $('#txn-select-all-bar').hide().html(
        'All <strong id="txn-page-count"></strong> transactions on this page are selected.'
        + ' &nbsp;<a href="#" onclick="selectAllPages();return false">Select all <strong id="txn-all-count">'
        + '</strong> transactions in this period</a>'
        + ' &nbsp;&mdash;&nbsp;<a href="#" onclick="clearSelection();return false">Clear selection</a>'
    );
    updateTxnSel();
}

function updateTxnSel() {
    var n = selectedTxns.size;
    $('#txn-sel-badge').toggle(n > 0).text(n + ' selected');
    $('#btn-recategorize').prop('disabled', n === 0);
}

async function recategorizeSelected(mode) {
    if (!mode) mode = 'classify';
    var ids = Array.from(selectedTxns);
    if (!ids.length) return;
    var labels = {classify: 'Set Category', destination: 'Set Destination', both: 'Set Both'};
    var desc = {classify: 'classify categories', destination: 'match destination accounts', both: 'classify categories and match destinations'};
    var modeLabel = labels[mode] || 'Process';
    var modeDesc = desc[mode] || 'process';
    showModal(modeLabel + ' transactions',
        'Run AI to ' + modeDesc + ' on ' + ids.length + ' selected transaction(s)? Existing data will be replaced.',
        async function () {
            txnFlash('<i class="fa fa-spinner fa-spin"></i> Submitting ' + ids.length + ' job(s)&hellip;', 'info');
            var res = await fetch('/batch/run', {
                method: 'POST',
                headers: {'Content-Type': 'application/json'},
                body: JSON.stringify({filter: {transaction_ids: ids}, force: true, mode: mode})
            });
            if (!res.ok) {txnFlash('<i class="fa fa-times"></i> Error: ' + esc(await res.text()), 'danger'); return;}
            var d = await res.json();
            txnFlash('<i class="fa fa-check"></i> ' + d.enqueued + ' job(s) enqueued. Switch to Jobs to monitor progress.', 'success');
            selectedTxns.clear(); updateTxnSel();
        });
}

// Initialise flatpickr on the transaction date range inputs.
// Using flatpickr avoids the browser's native date picker, which in Firefox
// renders weekend day numbers in red with no CSS override available.
(function () {
    var end = new Date(), start = new Date();
    start.setDate(start.getDate() - 30);
    var fpOpts = {dateFormat: 'Y-m-d', allowInput: true, disableMobile: true};
    flatpickr('#txn-start', Object.assign({}, fpOpts, {defaultDate: start}));
    flatpickr('#txn-end',   Object.assign({}, fpOpts, {defaultDate: end}));
})();

// ─── Review ────────────────────────────────────────────────────────────────
var reviewCategories = [];
var reviewAccounts = [];     // expense accounts for destination matching (empty when disabled)
var reviewGroupCounter = 0;
var reviewGroupMap = {}; // gi -> group object for the current rendered batch
var reviewDestFocused = {}; // gi -> bool, tracks which destination fields have been focused for the first time
var pendingAccounts = []; // [{name}], new accounts typed on this page (not yet saved to Firefly)
var allReviewGroups = []; // the live queue; submitted groups are removed, skipped groups cycle to end
var REVIEW_BATCH_SIZE = 8;

async function loadReview(silent) {
    var body = document.getElementById('review-body');
    var actionBar = document.getElementById('review-action-bar');
    var destBody = document.getElementById('review-dest-body');
    var transferBody = document.getElementById('transfer-body');
    var destSection = document.getElementById('review-section-destination');
    var transferSection = document.getElementById('review-section-transfers');
    if (!silent) {
        body.innerHTML = '<p><i class="fa fa-spinner fa-spin"></i> Loading&hellip;</p>';
        actionBar.style.display = 'none';
        hideReviewProcessingBar();
        destBody.innerHTML = '<p class="text-muted"><i class="fa fa-spinner fa-spin"></i> Loading&hellip;</p>';
        transferBody.innerHTML = '<p class="text-muted"><i class="fa fa-spinner fa-spin"></i> Loading&hellip;</p>';
    }
    try {
        var catRes = fetch('/api/categories');
        var reviewRes = fetch('/api/review');
        var destMatch = savedConfig.destination_match_enabled;
        var acctRes = destMatch ? fetch('/api/accounts') : null;
        catRes = await catRes;
        reviewRes = await reviewRes;
        if (acctRes) acctRes = await acctRes;

        if (catRes.status === 503 || reviewRes.status === 503) {
            body.innerHTML = '<div class="alert alert-warning"><i class="fa fa-warning"></i> Firefly III is not configured. Set credentials in Settings.</div>';
            return;
        }
        if (!catRes.ok) throw new Error(await catRes.text());
        if (!reviewRes.ok) throw new Error(await reviewRes.text());

        reviewCategories = await catRes.json();
        allReviewGroups = (await reviewRes.json()) || [];
        reviewAccounts = (destMatch && acctRes && acctRes.ok) ? (await acctRes.json()) || [] : [];

        updateReviewBadge();
        renderCurrentBatch();
    } catch (e) {
        body.innerHTML = '<div class="alert alert-danger"><i class="fa fa-times-circle"></i> ' + esc(e.message) + '</div>';
    }
}

function renderCurrentBatch() {
    var body = document.getElementById('review-body');
    var actionBar = document.getElementById('review-action-bar');
    var destBody = document.getElementById('review-dest-body');
    var destSection = document.getElementById('review-section-destination');
    var transferBody = document.getElementById('transfer-body');
    var transferSection = document.getElementById('review-section-transfers');

    // Split groups by outcome.
    var reviewGroups = allReviewGroups.filter(function (g) {
        return g.outcome !== 'TRANSFER_CATEGORY';
    });
    var transferGroups = allReviewGroups.filter(function (g) {
        return g.outcome === 'TRANSFER_CATEGORY';
    });
    var batch = reviewGroups.slice(0, REVIEW_BATCH_SIZE);

    reviewGroupCounter = 0;
    reviewGroupMap = {};
    reviewDestFocused = {};
    pendingAccounts = [];

    // Review Categories section (always show, may be empty).
    if (batch.length) {
        body.innerHTML = renderReviewGroups(batch);
        actionBar.style.display = '';
        updateSubmitBar();
    } else if (transferGroups.length) {
        body.innerHTML = '<p class="text-muted">No transactions need category review!</p>';
        actionBar.style.display = 'none';
    } else {
        body.innerHTML = '<p class="text-muted">No transactions need category review &mdash; all caught up!</p>';
        actionBar.style.display = 'none';
    }

    // Review Destination Account section — always shown when destination matching is enabled.
    var destMatch = savedConfig.destination_match_enabled;
    if (destMatch) {
        destSection.style.display = '';
        // Show destination-review cards: only groups where destination isn't set.
        // For now, reuse the same cards but add a contextual message.
        destBody.innerHTML = '<p class="text-muted">Destination accounts are reviewed together with categories above &mdash; each review card includes a destination field. Use the <strong>Set Destination</strong> action on the Transactions page to batch-assign destinations.</p>';
    } else {
        destSection.style.display = 'none';
    }

    // Transfer section — always visible when destination matching is enabled.
    if (transferGroups.length) {
        transferSection.style.display = '';
        transferBody.innerHTML = renderTransferSection(transferGroups);
    } else {
        transferSection.style.display = destMatch ? '' : 'none';
        transferBody.innerHTML = '<p class="text-muted">No transactions flagged for transfer conversion.</p>';
    }

    updateProgressIndicator();
    updateReviewBadge();
    populateTransferDestinations();
}

function updateProgressIndicator() {
    var el = document.getElementById('review-progress');
    if (!el) return;
    var n = allReviewGroups.filter(function (g) {
        return g.outcome !== 'TRANSFER_CATEGORY';
    }).length;
    el.textContent = n > REVIEW_BATCH_SIZE
        ? 'Showing first ' + REVIEW_BATCH_SIZE + ' of ' + n + ' groups'
        : '';
}

function updateReviewBadge() {
    var n = allReviewGroups.length;
    $('#review-count-badge').toggle(n > 0).text(n);
}

function showReviewProcessingBar(txnCount) {
    document.getElementById('review-processing-status').innerHTML =
        '<span class="text-muted"><i class="fa fa-spinner fa-spin"></i> Applying '
        + txnCount + ' transaction' + (txnCount === 1 ? '' : 's') + '&hellip;</span>';
}

function updateReviewProcessingBar(success, errors) {
    var el = document.getElementById('review-processing-status');
    if (errors === 0) {
        el.innerHTML = '<span class="text-success"><i class="fa fa-check-circle"></i> '
            + success + ' transaction' + (success === 1 ? '' : 's') + ' categorized.</span>';
    } else {
        el.innerHTML = '<span class="text-warning"><i class="fa fa-warning"></i> '
            + success + ' categorized, ' + errors + ' failed.</span>';
    }
    setTimeout(function () { el.innerHTML = ''; }, 5000);
}

function hideReviewProcessingBar() {
    document.getElementById('review-processing-status').innerHTML = '';
}

function resolveGroupLabel(g) {
    var dest = (g.destination_name || '').trim();
    var noName = !dest || dest.toLowerCase() === '(no name)';
    var label = noName ? (g.description || '(unknown)') : dest;
    var sub = (!noName && g.description && g.description !== dest) ? g.description : '';
    return {label: label, sub: sub};
}

function renderReviewGroups(groups) {
    var needsReview = groups.filter(function (g) { return g.outcome === 'NEEDS_REVIEW'; });
    var assumed = groups.filter(function (g) { return g.outcome === 'ASSUMED'; });
    var html = '';

    if (needsReview.length) {
        html += '<p class="review-section-header">'
            + '<i class="fa fa-flag text-danger"></i> <strong>Needs Review</strong>'
            + ' <span class="text-muted">&mdash; The AI could not classify these.</span></p>'
            + '<div class="review-groups-grid" id="grid-needs-review">'
            + needsReview.map(function (g) { return renderReviewGroup(g, false); }).join('')
            + '</div>';
    }

    if (assumed.length) {
        html += '<p class="review-section-header"' + (needsReview.length ? ' style="margin-top:20px"' : '') + '>'
            + '<i class="fa fa-question-circle text-warning"></i> <strong>AI Assumed</strong>'
            + ' <span class="text-muted">&mdash; The AI made a best guess. Confirm or correct.</span></p>'
            + '<div class="review-groups-grid" id="grid-assumed">'
            + assumed.map(function (g) { return renderReviewGroup(g, true); }).join('')
            + '</div>';
    }

    return html;
}

// Render the transfer conversion section. Uses the same counter/map as
// renderReviewGroups so gi values are unique across both sections.
function renderTransferSection(groups) {
    if (!groups.length) return '';
    return '<p class="review-section-header">'
        + '<i class="fa fa-exchange text-info"></i> <strong>Categorized as &quot;Transfers&quot;</strong>'
        + ' <span class="text-muted">&mdash; Convert to actual transfer transactions by selecting a destination asset account.</span></p>'
        + '<div class="review-groups-grid" id="grid-transfers">'
        + groups.map(function (g) { return renderTransferGroup(g); }).join('')
        + '</div>';
}

function renderTransferGroup(g) {
    var gi = reviewGroupCounter++;
    reviewGroupMap[gi] = g;
    var resolved = resolveGroupLabel(g);
    var count = (g.transactions || []).length;
    var idsJson = JSON.stringify((g.transactions || []).map(function (t) { return t.id; }));

    var txnRows = (g.transactions || []).map(function (t, idx) {
        var date = t.date ? t.date.substring(0, 10) : '—';
        var amount = isNaN(parseFloat(t.amount)) ? '—' : parseFloat(t.amount).toFixed(2);
        return '<tr>'
            + '<td style="white-space:nowrap;font-size:12px;padding:3px 6px">' + esc(date) + '</td>'
            + '<td class="text-right" style="font-size:12px;padding:3px 6px">' + amount + '</td>'
            + '</tr>';
    }).join('');

    // Individual transaction rows for expanded view.
    var indivRows = (g.transactions || []).map(function (t, idx) {
        var date = t.date ? t.date.substring(0, 10) : '—';
        var amount = isNaN(parseFloat(t.amount)) ? '—' : parseFloat(t.amount).toFixed(2);
        return '<tr>'
            + '<td style="white-space:nowrap;font-size:12px;padding:3px 6px">' + esc(date) + '</td>'
            + '<td class="text-right" style="font-size:12px;padding:3px 6px">' + amount + '</td>'
            + '<td style="padding:3px 6px;width:100%">'
            + '<select class="form-control input-sm" id="transfer-dest-' + gi + '-' + idx + '"'
            + ' data-txn-id="' + esc(t.id) + '" onchange="updateTransferButtons()">'
            + '<option value="">— select —</option>'
            + '</select>'
            + '</td>'
            + '</tr>';
    }).join('');

    var sub = resolved.sub
        ? ' <small style="font-weight:normal;color:#999">' + esc(resolved.sub) + '</small>' : '';

    var hasMultiple = count > 1;

    return '<div class="box box-info review-group" id="review-group-' + gi + '"'
        + ' data-ids="' + esc(idsJson) + '" data-count="' + count + '"'
        + ' data-desc="' + esc(g.description || g.destination_name || '') + '">'
        + '<div class="box-header with-border review-group-header">'
        + '<span class="review-group-title">' + esc(resolved.label) + sub
        + ' <span class="label label-info" style="font-size:10px;font-weight:normal;margin-left:3px">'
        + count + '</span></span>'
        + '<button type="button" class="btn btn-xs btn-default review-skip-btn" onclick="skipReviewGroup(' + gi + ')" title="Skip — review later">'
        + '<i class="fa fa-clock-o"></i> Skip'
        + '</button>'
        + '</div>'
        + '<div class="box-body review-group-body">'
        // Collapsed view: summary table + single destination input.
        + '<div id="transfer-collapsed-' + gi + '">'
        + '<table class="table table-condensed" style="margin-bottom:6px">'
        + '<tbody>' + txnRows + '</tbody>'
        + '</table>'
        + '<div style="display:flex;gap:8px;align-items:center;flex-wrap:wrap">'
        + '<div style="flex:1;min-width:180px">'
        + '<select class="form-control input-sm" id="transfer-dest-' + gi + '" onchange="updateTransferButtons()">'
        + '<option value="">— select destination account —</option>'
        + '</select>'
        + '</div>'
        + '<span id="suggest-result-' + gi + '" style="font-size:12px"></span>'
        + '</div>'
        + (hasMultiple ? '<div style="margin-top:4px"><a href="#" style="font-size:12px" onclick="toggleTransferExpand(' + gi + ');return false"><i class="fa fa-list-ul"></i> Set destinations individually</a></div>' : '')
        + '<div style="margin-top:8px">'
        + '<button type="button" class="btn btn-primary btn-sm" onclick="convertToTransfer(' + gi + ')"'
        + ' id="btn-convert-' + gi + '" disabled>'
        + '<i class="fa fa-exchange"></i> Convert to Transfer'
        + '</button>'
        + '<span id="convert-status-' + gi + '" style="margin-left:8px;font-size:12px"></span>'
        + '</div>'
        + '</div>'
        // Expanded view: individual destinations per transaction.
        + (hasMultiple ? '<div id="transfer-expanded-' + gi + '" style="display:none">'
        + '<table class="table table-condensed" style="margin-bottom:6px">'
        + '<thead><tr><th>Date</th><th class="text-right">Amount</th><th>Destination Account</th></tr></thead>'
        + '<tbody>' + indivRows + '</tbody>'
        + '</table>'
        + '<div style="margin-top:4px"><a href="#" style="font-size:12px" onclick="toggleTransferExpand(' + gi + ');return false"><i class="fa fa-compress"></i> Use same destination for all</a></div>'
        + '<div style="margin-top:8px">'
        + '<button type="button" class="btn btn-primary btn-sm" onclick="convertToTransfer(' + gi + ')"'
        + ' id="btn-convert-exp-' + gi + '" disabled>'
        + '<i class="fa fa-exchange"></i> Convert to Transfer'
        + '</button>'
        + '<span id="convert-status-exp-' + gi + '" style="margin-left:8px;font-size:12px"></span>'
        + '</div>'
        + '</div>' : '')
        + '</div>'
        + '</div>';
}

// Toggle between grouped and individual destination mode for a transfer group.
function toggleTransferExpand(gi) {
    var collapsed = document.getElementById('transfer-collapsed-' + gi);
    var expanded = document.getElementById('transfer-expanded-' + gi);
    if (!collapsed || !expanded) return;
    var showing = collapsed.style.display !== 'none';
    collapsed.style.display = showing ? 'none' : '';
    expanded.style.display = showing ? '' : 'none';
    if (showing) {
        // Copy the main select value to all individual selects.
        var mainSel = document.getElementById('transfer-dest-' + gi);
        if (mainSel) {
            var sels = document.querySelectorAll('[id^="transfer-dest-' + gi + '-"]');
            sels.forEach(function (s) {
                if (!s.value) s.value = mainSel.value;
            });
        }
    }
    updateTransferButtons();
}

// Populate transfer destination dropdowns with asset accounts and auto-suggest.
function populateTransferDestinations() {
    var groups = document.querySelectorAll('[id^="review-group-"][data-desc]');
    if (!groups.length) return;
    fetch('/api/accounts?type=asset')
        .then(function (r) { return r.ok ? r.json() : []; })
        .then(function (accts) {
            var optionsHtml = accts.map(function (a) {
                return '<option value="' + esc(a.id) + '">' + esc(a.name) + '</option>';
            }).join('');

            // Populate all transfer destination selects.
            var selects = document.querySelectorAll('[id^="transfer-dest-"]');
            selects.forEach(function (sel) {
                var currentVal = sel.value;
                sel.innerHTML = '<option value="">— select —</option>' + optionsHtml;
                sel.value = currentVal;
            });

            // Auto-suggest for each group.
            groups.forEach(function (el) {
                var gi = el.id.replace('review-group-', '');
                var desc = el.getAttribute('data-desc');
                if (desc) autoSuggestTransfer(gi, desc);
            });
        });
}

// Auto-suggest in the background (no button click needed).
async function autoSuggestTransfer(gi, desc) {
    var result = document.getElementById('suggest-result-' + gi);
    if (!result) return;
    result.innerHTML = '<i class="fa fa-spinner fa-spin"></i> Suggesting&hellip;';
    try {
        var res = await fetch('/api/transfers/suggest?description=' + encodeURIComponent(desc));
        if (!res.ok) throw new Error(await res.text());
        var s = await res.json();
        if (s.account_id && s.account_name) {
            var sel = document.getElementById('transfer-dest-' + gi);
            if (sel && !sel.value) {
                sel.value = s.account_id;
            }
            // Also pre-fill individual selects in expanded view.
            var indivSels = document.querySelectorAll('[id^="transfer-dest-' + gi + '-"]');
            indivSels.forEach(function (s2) {
                if (!s2.value) s2.value = s.account_id;
            });
            result.innerHTML = '<span class="text-success" style="font-size:11px"><i class="fa fa-check-circle"></i> '
                + (s.source === 'history' ? 'History: ' : 'AI: ')
                + '<strong>' + esc(s.account_name) + '</strong></span>';
        } else {
            result.innerHTML = '<span class="text-muted" style="font-size:11px">Could not determine.</span>';
        }
    } catch (e) {
        result.innerHTML = '<span class="text-muted" style="font-size:11px">Suggestion unavailable.</span>';
    }
    updateTransferButtons();
}

function updateTransferButtons() {
    document.querySelectorAll('[id^="transfer-dest-"]').forEach(function (sel) {
        var idParts = sel.id.replace('transfer-dest-', '').split('-');
        var gi = idParts[0];
        var btn = document.getElementById('btn-convert-' + gi);
        var mainSel = document.getElementById('transfer-dest-' + gi);
        if (btn) btn.disabled = !(mainSel && mainSel.value);
        var btnExp = document.getElementById('btn-convert-exp-' + gi);
        if (btnExp) {
            var allFilled = true;
            var indivSels = document.querySelectorAll('[id^="transfer-dest-' + gi + '-"]');
            indivSels.forEach(function (s) { if (!s.value) allFilled = false; });
            btnExp.disabled = !allFilled;
        }
    });
}

async function convertToTransfer(gi) {
    var el = document.getElementById('review-group-' + gi);
    var ids = JSON.parse(el.getAttribute('data-ids') || '[]');
    var expanded = document.getElementById('transfer-expanded-' + gi);
    var isExpanded = expanded && expanded.style.display !== 'none';

    var destMap = {}; // txnID → accountID (the select values ARE the account IDs)
    if (isExpanded) {
        var indivSels = document.querySelectorAll('[id^="transfer-dest-' + gi + '-"]');
        indivSels.forEach(function (sel) {
            var txnID = sel.getAttribute('data-txn-id');
            destMap[txnID] = sel.value;
        });
    } else {
        var mainSel = document.getElementById('transfer-dest-' + gi);
        var destID = mainSel ? mainSel.value : '';
        ids.forEach(function (id) { destMap[id] = destID; });
    }

    var btnId = isExpanded ? 'btn-convert-exp-' + gi : 'btn-convert-' + gi;
    var statusId = isExpanded ? 'convert-status-exp-' + gi : 'convert-status-' + gi;
    var btn = document.getElementById(btnId);
    var status = document.getElementById(statusId);

    btn.disabled = true;
    status.innerHTML = '<i class="fa fa-spinner fa-spin"></i> Converting&hellip;';

    var errors = 0;
    for (var i = 0; i < ids.length; i++) {
        var destID = destMap[ids[i]];
        if (!destID) { errors++; continue; }
        try {
            var res = await fetch('/api/transactions/' + encodeURIComponent(ids[i]) + '/convert-to-transfer', {
                method: 'POST',
                headers: {'Content-Type': 'application/json'},
                body: JSON.stringify({destination_id: destID})
            });
            if (!res.ok) { errors++; }
        } catch (e) { errors++; }
    }

    if (errors === 0) {
        status.innerHTML = '<span class="text-success"><i class="fa fa-check-circle"></i> Converted ' + ids.length + ' transaction(s).</span>';
        var g = reviewGroupMap[gi];
        if (g) {
            var idx = allReviewGroups.indexOf(g);
            if (idx !== -1) allReviewGroups.splice(idx, 1);
        }
        setTimeout(function () {
            el.remove();
            updateReviewBadge();
            // Hide the transfer box if no more transfer groups remain.
            if (!document.querySelector('#grid-transfers .review-group')) {
                document.getElementById('review-section-transfers').style.display = 'none';
            }
        }, 1500);
    } else {
        status.innerHTML = '<span class="text-danger"><i class="fa fa-times"></i> ' + errors + ' of ' + ids.length + ' failed.</span>';
    }
    btn.disabled = false;
}

function renderReviewGroup(g, isAssumed) {
    var gi = reviewGroupCounter++;
    reviewGroupMap[gi] = g;
    var resolved = resolveGroupLabel(g);
    var count = (g.transactions || []).length;
    var idsJson = JSON.stringify((g.transactions || []).map(function (t) { return t.id; }));

    var txnRows = (g.transactions || []).map(function (t) {
        var date = t.date ? t.date.substring(0, 10) : '—';
        var amount = isNaN(parseFloat(t.amount)) ? '—' : parseFloat(t.amount).toFixed(2);
        return '<tr>'
            + '<td style="white-space:nowrap;font-size:12px;padding:3px 6px">' + esc(date) + '</td>'
            + '<td class="text-right" style="font-size:12px;padding:3px 6px">' + amount + '</td>'
            + '</tr>';
    }).join('');

    var countBadgeCls = isAssumed ? 'label-warning' : 'label-danger';
    var sub = resolved.sub
        ? ' <small style="font-weight:normal;color:#999">' + esc(resolved.sub) + '</small>' : '';
    var aiHint = isAssumed && g.category_name
        ? '<p style="margin:0 0 5px;font-size:11px;color:#8a6d3b">'
            + '<i class="fa fa-magic"></i> AI guessed: <strong>' + esc(g.category_name) + '</strong></p>'
        : '';

    var catOptions = '<option value="" disabled selected hidden>—</option>'
        + reviewCategories.map(function (c) {
            var sel = (isAssumed && g.category_id && c.id === g.category_id) ? ' selected' : '';
            return '<option value="' + esc(c.id) + '"' + sel + '>' + esc(c.name) + '</option>';
        }).join('');

    // Destination account controls (only when enabled).
    var destHtml = '';
    if (reviewAccounts.length) {
        // Find the current destination name for pre-filling.
        var currentDestID = g.destination_account_id || '';
        var currentDestName = '';
        if (currentDestID) {
            for (var i = 0; i < reviewAccounts.length; i++) {
                if (reviewAccounts[i].id === currentDestID) {
                    currentDestName = reviewAccounts[i].name;
                    break;
                }
            }
        }

        // Pre-set data-matched-id if the current destination matches an existing account.
        var matchedAttr = '';
        if (currentDestID) {
            matchedAttr = ' data-matched-id="' + esc(currentDestID) + '"';
        }

        var acctDataOptions = reviewAccounts.map(function (a) {
            return '<option value="' + esc(a.name) + '" data-id="' + esc(a.id) + '">';
        }).join('')
            + pendingAccounts.map(function (a) {
                return '<option value="' + esc(a.name) + '" data-id="new:' + esc(a.name) + '">';
            }).join('');

        destHtml = '<div style="margin-top:8px" class="review-dest-container">'
            + '<label style="font-size:12px;font-weight:600;display:block;margin-bottom:3px">'
            + '<i class="fa fa-building-o"></i> Destination Account'
            + ' <span style="font-weight:400;color:#848484">(expense / payee)</span></label>'
            + '<div style="display:flex;gap:6px;align-items:center">'
            + '<input type="text" class="form-control input-sm" id="review-dest-' + gi + '"'
            + ' list="review-dest-list-' + gi + '" placeholder="Search or type a new account…"'
            + ' value="' + esc(currentDestName) + '"' + matchedAttr
            + ' onfocus="onReviewDestFocus(' + gi + ')"'
            + ' oninput="onReviewDestInput(' + gi + ')"'
            + ' onblur="onReviewDestBlur(' + gi + ')"'
            + ' style="flex:1">'
            + '<datalist id="review-dest-list-' + gi + '">' + acctDataOptions + '</datalist>'
            + '<span id="review-dest-badge-' + gi + '" class="label label-info"'
            + ' style="display:none;flex-shrink:0;font-size:10px;white-space:nowrap">'
            + '<i class="fa fa-plus-circle"></i> NEW</span>'
            + '</div>'
            + '</div>';
    }

    return '<div class="box box-default review-group" id="review-group-' + gi + '"'
        + ' data-ids="' + esc(idsJson) + '" data-count="' + count + '">'
        + '<div class="box-header with-border review-group-header">'
        + '<span class="review-group-title">' + esc(resolved.label) + sub
        + ' <span class="label ' + countBadgeCls + '" style="font-size:10px;font-weight:normal;margin-left:3px">'
        + count + '</span></span>'
        + '<button type="button" class="btn btn-xs btn-default review-skip-btn" onclick="skipReviewGroup(' + gi + ')" title="Skip — move to end of queue to review later">'
        + '<i class="fa fa-clock-o"></i> Skip'
        + '</button>'
        + '</div>'
        + '<div class="box-body review-group-body">'
        + aiHint
        + '<table class="table table-condensed" style="margin-bottom:6px">'
        + '<tbody>' + txnRows + '</tbody>'
        + '</table>'
        + '<label style="font-size:12px;font-weight:600;display:block;margin-bottom:3px">'
        + '<i class="fa fa-bookmark-o"></i> Category</label>'
        + '<select class="form-control input-sm" id="review-cat-' + gi + '" onchange="updateSubmitBar();onReviewCatChange(' + gi + ')">'
        + catOptions
        + '</select>'
        + destHtml
        + '</div>'
        + '</div>';
}

// On first focus: clear any pre-filled value so the user can start fresh.
// Subsequent focuses on the same field leave the user's input intact.
function onReviewDestFocus(gi) {
    var badge = document.getElementById('review-dest-badge-' + gi);
    if (badge) badge.style.display = 'none';
    if (!reviewDestFocused[gi]) {
        reviewDestFocused[gi] = true;
        var input = document.getElementById('review-dest-' + gi);
        if (input && input.value.trim()) {
            input.value = '';
            input.removeAttribute('data-matched-id');
        }
    }
    updateSubmitBar();
}

// On input: check if the typed name exactly matches an existing account and
// record its ID for later submission. The datalist provides native suggestions.
// The NEW badge is hidden while the user is typing.
function onReviewDestInput(gi) {
    var input = document.getElementById('review-dest-' + gi);
    var badge = document.getElementById('review-dest-badge-' + gi);
    if (!input) return;

    if (badge) badge.style.display = 'none';

    var val = input.value.trim();
    // Check if the typed name exactly matches an account in the datalist.
    var matchedID = '';
    var list = document.getElementById('review-dest-list-' + gi);
    if (list && val) {
        var options = list.querySelectorAll('option');
        for (var i = 0; i < options.length; i++) {
            if (options[i].value.toLowerCase() === val.toLowerCase()) {
                matchedID = options[i].getAttribute('data-id');
                break;
            }
        }
    }

    if (matchedID) {
        input.setAttribute('data-matched-id', matchedID);
    } else {
        input.removeAttribute('data-matched-id');
    }
    updateSubmitBar();
}

// On blur: if the user typed a value that doesn't match any existing account,
// show the NEW badge and add it to pending accounts so other groups can use it.
function onReviewDestBlur(gi) {
    var input = document.getElementById('review-dest-' + gi);
    var badge = document.getElementById('review-dest-badge-' + gi);
    if (!input) return;
    var val = input.value.trim();
    var matchedID = input.getAttribute('data-matched-id');

    if (!val || matchedID) {
        // Empty or exact match — hide badge.
        if (badge) badge.style.display = 'none';
    } else {
        // No match — show NEW badge and add to pending accounts.
        if (badge) badge.style.display = '';
        // Avoid duplicates in pendingAccounts.
        var exists = pendingAccounts.some(function (a) {
            return a.name.toLowerCase() === val.toLowerCase();
        });
        if (!exists) {
            pendingAccounts.push({name: val});
            refreshDatalists();
        }
    }
    updateSubmitBar();
}

// Refresh all destination datalists to include pending (not-yet-saved) accounts.
function refreshDatalists() {
    var pendingHTML = pendingAccounts.map(function (a) {
        return '<option value="' + esc(a.name) + '" data-id="new:' + esc(a.name) + '">';
    }).join('');
    var lists = document.querySelectorAll('[id^="review-dest-list-"]');
    lists.forEach(function (list) {
        // Remove stale pending options, then append fresh ones.
        var old = list.querySelectorAll('option[data-id^="new:"]');
        for (var i = 0; i < old.length; i++) { old[i].remove(); }
        list.insertAdjacentHTML('beforeend', pendingHTML);
    });
}

// When the category changes on a review card, hide the destination controls
// if "Transfers" is selected (transfers are handled in the Transfers section).
function onReviewCatChange(gi) {
    var sel = document.getElementById('review-cat-' + gi);
    if (!sel) return;
    var el = document.getElementById('review-group-' + gi);
    if (!el) return;
    var selectedText = sel.options[sel.selectedIndex] ? sel.options[sel.selectedIndex].text : '';
    var destContainer = el.querySelector('.review-dest-container');
    if (!destContainer) return;
    destContainer.style.display = (selectedText.toLowerCase() === 'transfers') ? 'none' : '';
}

function skipReviewGroup(gi) {
    var el = document.getElementById('review-group-' + gi);
    if (!el) return;
    // Move group to end of queue so it reappears after all other groups are reviewed
    var g = reviewGroupMap[gi];
    if (g) {
        var idx = allReviewGroups.indexOf(g);
        if (idx !== -1) {
            allReviewGroups.splice(idx, 1);
            allReviewGroups.push(g);
        }
    }
    el.remove();
    // Hide transfer section if the skipped group was the last transfer.
    if (!document.querySelector('#grid-transfers .review-group')) {
        document.getElementById('review-section-transfers').style.display = 'none';
    }
    if (!document.querySelector('.review-group')) {
        renderCurrentBatch();
    } else {
        updateSubmitBar();
        updateProgressIndicator();
    }
}

function updateSubmitBar() {
    // Only count review groups (needs-review + assumed), not transfers.
    var groups = document.querySelectorAll('#review-body .review-group');
    var total = groups.length;
    var categorized = 0;
    groups.forEach(function (el) {
        var gi = el.id.replace('review-group-', '');
        var sel = document.getElementById('review-cat-' + gi);
        if (sel && sel.value) categorized++;
    });
    var btn = document.getElementById('btn-submit-all');
    if (!btn) return;
    btn.disabled = categorized === 0;
    btn.innerHTML = '<i class="fa fa-check"></i> Apply ' + categorized + ' of ' + total
        + ' group' + (total === 1 ? '' : 's');
}

async function submitAllReview() {
    var toSubmit = Array.from(document.querySelectorAll('#review-body .review-group')).filter(function (el) {
        var gi = el.id.replace('review-group-', '');
        var sel = document.getElementById('review-cat-' + gi);
        return sel && sel.value;
    });
    if (!toSubmit.length) return;

    // Capture data and remove submitted groups from the queue before touching the DOM
    var submissions = toSubmit.map(function (el) {
        var gi = el.id.replace('review-group-', '');
        var sel = document.getElementById('review-cat-' + gi);
        var g = reviewGroupMap[gi];
        if (g) {
            var idx = allReviewGroups.indexOf(g);
            if (idx !== -1) allReviewGroups.splice(idx, 1);
        }
        var sub = {ids: JSON.parse(el.getAttribute('data-ids')), categoryId: sel.value};
        // Capture destination info from the single searchable field.
        var destInput = document.getElementById('review-dest-' + gi);
        if (destInput) {
            var matchedID = destInput.getAttribute('data-matched-id');
            var destName = destInput.value.trim();
            if (matchedID && matchedID.indexOf('new:') === 0) {
                // Pending account — treat as CREATE with the name.
                sub.destination_action = 'CREATE';
                sub.destination_name = matchedID.substring(4);
            } else if (matchedID) {
                sub.destination_action = 'MATCH';
                sub.destination_id = matchedID;
            } else if (destName) {
                sub.destination_action = 'CREATE';
                sub.destination_name = destName;
            }
        }
        return sub;
    });

    // Remove submitted cards from DOM immediately
    toSubmit.forEach(function (el) { el.remove(); });
    updateReviewBadge();

    // Show processing banner and advance to the next batch straight away
    var totalTxns = submissions.reduce(function (sum, s) { return sum + s.ids.length; }, 0);
    showReviewProcessingBar(totalTxns);

    if (!document.querySelector('.review-group')) {
        renderCurrentBatch();
    } else {
        updateSubmitBar();
        updateProgressIndicator();
    }

    // Fire all categorize calls in parallel
    var allCalls = submissions.flatMap(function (s) {
        return s.ids.map(function (id) {
            var body = {category_id: s.categoryId};
            if (s.destination_action) {
                body.destination_action = s.destination_action;
                if (s.destination_action === 'MATCH') body.destination_id = s.destination_id;
                if (s.destination_action === 'CREATE') body.destination_name = s.destination_name;
            }
            return fetch('/api/transactions/' + encodeURIComponent(id) + '/categorize', {
                method: 'PUT',
                headers: {'Content-Type': 'application/json'},
                body: JSON.stringify(body)
            }).then(function (res) { return res.ok; }).catch(function () { return false; });
        });
    });

    var results = await Promise.all(allCalls);
    var errors = results.filter(function (ok) { return !ok; }).length;
    updateReviewProcessingBar(results.length - errors, errors);
}

// ─── Categories ────────────────────────────────────────────────────────────
function buildCategoriesHTML(cats) {
    if (!cats || !cats.length) {
        return '<p class="text-muted">No categories found in Firefly III.</p>';
    }
    var html = '<div style="margin-bottom:12px">';
    cats.forEach(function (c) {
        var isTransfer = c.name.toLowerCase() === 'transfers';
        var chipCls = isTransfer ? ' cat-chip-transfer' : '';
        var icon = isTransfer
            ? '<i class="fa fa-exchange" style="margin-right:5px"></i>'
            : '<i class="fa fa-bookmark-o" style="color:#3c8dbc;margin-right:5px"></i>';
        html += '<span class="cat-chip' + chipCls + '">' + icon
            + esc(c.name)
            + (c.notes ? ' <span class="cat-notes">&mdash; ' + esc(c.notes) + '</span>' : '')
            + '</span>';
    });
    html += '</div><p class="text-muted"><small><i class="fa fa-info-circle"></i> '
        + cats.length + ' categories available to the AI.</small></p>';
    return html;
}

function renderCachedCategories() {
    if (!cachedCategories) return;
    document.getElementById('cat-body').innerHTML = cachedCategories.html;
}

async function loadCategories() {
    var catBody = document.getElementById('cat-body');
    // Only show spinner if nothing is rendered (first load).
    if (!cachedCategories) {
        catBody.innerHTML = '<p><i class="fa fa-spinner fa-spin"></i> Loading&hellip;</p>';
    }

    try {
        var res = await fetch('/api/categories');
        if (res.status === 503) {
            catBody.innerHTML = '<div class="alert alert-warning"><i class="fa fa-warning"></i> Firefly III is not configured. Set credentials in Settings.</div>';
            cachedCategories = null;
            return;
        }
        if (!res.ok) throw new Error(await res.text());
        var cats = await res.json();
        var html = buildCategoriesHTML(cats);
        catBody.innerHTML = html;
        cachedCategories = {html: html, data: cats};
    } catch (e) {
        catBody.innerHTML = '<div class="alert alert-danger"><i class="fa fa-times-circle"></i> ' + esc(e.message) + '</div>';
        cachedCategories = null;
    }
}

// ─── Accounts ──────────────────────────────────────────────────────────────
function buildAccountsHTML(accts) {
    if (!accts || !accts.length) {
        return '<p class="text-muted">No expense accounts found in Firefly III. The AI will only be able to create new accounts when destination matching is enabled.</p>';
    }
    var ahtml = '<div style="margin-bottom:12px">';
    accts.forEach(function (a) {
        ahtml += '<span class="cat-chip"><i class="fa fa-building-o" style="color:#3c8dbc;margin-right:5px"></i>'
            + esc(a.name) + '</span>';
    });
    ahtml += '</div><p class="text-muted"><small><i class="fa fa-info-circle"></i> '
        + accts.length + ' expense account' + (accts.length === 1 ? '' : 's')
        + ' that the AI can match against.</small></p>';
    return ahtml;
}

function renderCachedAccounts() {
    if (!cachedAccounts) return;
    document.getElementById('acct-body').innerHTML = cachedAccounts.html;
}

async function loadAccounts() {
    var acctBody = document.getElementById('acct-body');
    // Only show spinner if nothing is rendered (first load).
    if (!cachedAccounts) {
        acctBody.innerHTML = '<p><i class="fa fa-spinner fa-spin"></i> Loading&hellip;</p>';
    }

    try {
        var ar = await fetch('/api/accounts');
        if (ar.status === 503) {
            acctBody.innerHTML = '<div class="alert alert-warning"><i class="fa fa-warning"></i> Firefly III is not configured. Set credentials in Settings.</div>';
            cachedAccounts = null;
            return;
        }
        if (!ar.ok) throw new Error(await ar.text());
        var accts = await ar.json();
        var html = buildAccountsHTML(accts);
        acctBody.innerHTML = html;
        cachedAccounts = {html: html, data: accts};
    } catch (e) {
        acctBody.innerHTML = '<div class="alert alert-warning"><i class="fa fa-warning"></i> Could not load accounts: ' + esc(e.message) + '</div>';
        cachedAccounts = null;
    }
}

// ─── Settings ──────────────────────────────────────────────────────────────
var savedConfig = {};

// Ensures a URL has a protocol prefix, defaulting to https://.
function normalizeURL(url) {
    if (!url) return url;
    if (!/^https?:\/\//i.test(url)) return 'https://' + url;
    return url;
}

async function loadSettings() {
    settingsFlash('', '');
    try {
        var d = await (await fetch('/api/config')).json();
        savedConfig = d;
        $('#cfg-firefly-url').val(d.firefly_url || '');
        $('#cfg-firefly-token').val('');
        $('#token-hint').toggle(!!d.firefly_token_set);
        $('#cfg-provider').val(d.ai_provider || 'openai');
        $('#cfg-openai-key').val('');
        $('#openai-key-hint').toggle(!!d.openai_key_set);
        $('#cfg-openai-model').val(d.openai_model || '');
        $('#cfg-openai-base-url').val(d.openai_base_url || '');
        $('#cfg-gemini-key').val('');
        $('#gemini-key-hint').toggle(!!d.gemini_key_set);
        $('#cfg-gemini-model').val(d.gemini_model || '');
        $('#cfg-deepseek-key').val('');
        $('#deepseek-key-hint').toggle(!!d.deepseek_key_set);
        $('#cfg-deepseek-model').val(d.deepseek_model || '');
        $('#cfg-tag-prefix').val(d.tag_prefix || '');
        $('#cfg-custom-context').val(d.custom_system_context || '');
        $('#cfg-destination-match').prop('checked', !!d.destination_match_enabled);
        $('#cfg-history-context-limit').val(d.history_context_limit > 0 ? d.history_context_limit : '');
        $('#cfg-history-lookback-days').val(d.history_lookback_days || '');
        $('#cfg-worker-concurrency').val(d.worker_concurrency || '');
        $('#cfg-batch-concurrency').val(d.batch_concurrency || '');
        onProviderChange();
        $('#settings-badge').toggle(!d.configured);
        if (!d.configured) {
            settingsFlash('<i class="fa fa-exclamation-triangle"></i> Not fully configured &mdash; fill in the required fields below and save.', 'warning');
        } else {
            settingsFlash('<i class="fa fa-check-circle"></i> Configured and ready.', 'success');
        }
    } catch (e) {
        settingsFlash('<i class="fa fa-times-circle"></i> Failed to load configuration: ' + esc(e.message), 'danger');
    }
}

function settingsFlash(msg, type) {
    var el = document.getElementById('settings-flash');
    if (!msg) {el.innerHTML = ''; return;}
    el.innerHTML = '<div class="row"><div class="col-md-12"><div class="alert alert-' + type + '">' + msg + '</div></div></div>';
}

function onProviderChange() {
    var p = $('#cfg-provider').val();
    $('#section-openai').toggle(p === 'openai');
    $('#section-gemini').toggle(p === 'gemini');
    $('#section-deepseek').toggle(p === 'deepseek');
}

async function testConnection() {
    // Normalize the URL in place before testing.
    var url = normalizeURL($('#cfg-firefly-url').val().trim());
    if (url) $('#cfg-firefly-url').val(url);
    var token = $('#cfg-firefly-token').val();

    $('#btn-test').prop('disabled', true);
    $('#test-result').html('<i class="fa fa-spinner fa-spin"></i> Testing&hellip;').removeClass('text-success text-danger').addClass('text-muted');
    try {
        // Send current field values so the test works before saving.
        var body = {};
        if (url)   body.firefly_url   = url;
        if (token) body.firefly_token = token;

        var d = await (await fetch('/api/config/test', {
            method: 'POST',
            headers: {'Content-Type': 'application/json'},
            body: JSON.stringify(body)
        })).json();

        if (d.ok) {
            var msg = '<i class="fa fa-check-circle"></i> Connected &mdash; ' + d.categories + ' categories found';
            // Prompt to save if the current values haven't been persisted yet.
            var urlChanged   = url && url !== (savedConfig.firefly_url || '');
            var tokenChanged = !!token;
            if (urlChanged || tokenChanged) {
                msg += ' &mdash; <a href="#" onclick="saveSettings();return false">'
                    + '<i class="fa fa-floppy-o"></i> Save settings</a> to apply.';
            }
            $('#test-result').html(msg).removeClass('text-muted text-danger').addClass('text-success');
        } else {
            $('#test-result').html('<i class="fa fa-times-circle"></i> ' + esc(d.error))
                .removeClass('text-muted text-success').addClass('text-danger');
        }
    } catch (e) {
        $('#test-result').html('<i class="fa fa-times-circle"></i> ' + esc(e.message))
            .removeClass('text-muted text-success').addClass('text-danger');
    } finally {
        $('#btn-test').prop('disabled', false);
    }
}

async function saveSettings() {
    var p = $('#cfg-provider').val();
    var payload = {ai_provider: p};
    var ffUrl = normalizeURL($('#cfg-firefly-url').val().trim()), ffToken = $('#cfg-firefly-token').val();
    if (ffUrl) { $('#cfg-firefly-url').val(ffUrl); payload.firefly_url = ffUrl; }
    if (ffToken) payload.firefly_token = ffToken;
    if (p === 'openai') {
        var k = $('#cfg-openai-key').val(), m = $('#cfg-openai-model').val().trim(), b = $('#cfg-openai-base-url').val().trim();
        if (k) payload.openai_api_key = k;
        if (m) payload.openai_model = m;
        payload.openai_base_url = b;
    } else if (p === 'gemini') {
        var k = $('#cfg-gemini-key').val(), m = $('#cfg-gemini-model').val().trim();
        if (k) payload.gemini_api_key = k;
        if (m) payload.gemini_model = m;
    } else if (p === 'deepseek') {
        var k = $('#cfg-deepseek-key').val(), m = $('#cfg-deepseek-model').val().trim();
        if (k) payload.deepseek_api_key = k;
        if (m) payload.deepseek_model = m;
    }
    var tag = $('#cfg-tag-prefix').val().trim();
    if (tag) payload.tag_prefix = tag;
    // Always send custom_system_context (null vs "" distinction: null = don't change, "" = clear)
    payload.custom_system_context = $('#cfg-custom-context').val();
    payload.destination_match_enabled = $('#cfg-destination-match').prop('checked');
    var hcl = parseInt($('#cfg-history-context-limit').val(), 10);
    if (!isNaN(hcl) && hcl > 0) payload.history_context_limit = hcl;
    var hld = parseInt($('#cfg-history-lookback-days').val(), 10);
    if (!isNaN(hld) && hld > 0) payload.history_lookback_days = hld;
    var wc = parseInt($('#cfg-worker-concurrency').val(), 10);
    if (!isNaN(wc) && wc > 0) payload.worker_concurrency = wc;
    var bc = parseInt($('#cfg-batch-concurrency').val(), 10);
    if (!isNaN(bc) && bc > 0) payload.batch_concurrency = bc;
    $('#save-status').text('Saving…');
    try {
        var res = await fetch('/api/config', {
            method: 'PUT',
            headers: {'Content-Type': 'application/json'}, body: JSON.stringify(payload)
        });
        if (!res.ok) throw new Error(await res.text());
        $('#save-status').text('');
        // Invalidate caches — config may have changed.
        cachedCategories = null;
        cachedAccounts = null;
        loadSettings();
        applyTheme();
    } catch (e) {
        $('#save-status').html('<span class="text-danger">' + esc(e.message) + '</span>');
    }
}

// ─── Modal ─────────────────────────────────────────────────────────────────
function showModal(title, body, onConfirm) {
    $('#modal-title').text(title);
    $('#modal-body-text').text(body);
    $('#modal-confirm-btn').off('click').on('click', function () {
        $('#confirmModal').modal('hide');
        onConfirm();
    });
    $('#confirmModal').modal('show');
}

// ─── Utilities ─────────────────────────────────────────────────────────────
function trunc(s, n) {return s && s.length > n ? s.substring(0, n) + '…' : (s || '');}
function esc(s) {
    if (s == null) return '';
    return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
}
