// ─── Sidebar collapse persistence ──────────────────────────────────────────
$(document).on('collapsed.pushMenu', function () {
    localStorage.setItem('ff3ai_sidebar_collapsed', 'true');
});
$(document).on('expanded.pushMenu', function () {
    localStorage.setItem('ff3ai_sidebar_collapsed', 'false');
});

// ─── State ─────────────────────────────────────────────────────────────────
var jobs = {};
var pendingDryRunCount = 0;
var txnPage = 1, txnTotalPages = 1, txnTotal = 0;
var selectedTxns = new Set();

// ─── Tab navigation ─────────────────────────────────────────────────────────
var TAB_META = {
    jobs: {title: 'Jobs', sub: 'Recent classification activity'},
    transactions: {title: 'Transactions', sub: 'Select and re-categorize transactions'},
    categories: {title: 'Categories', sub: 'Categories available to the AI'},
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
    if (name === 'categories') loadCategories();
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
(async function () {
    try {
        var d = await (await fetch('/api/config')).json();
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

    tr.innerHTML =
        '<td style="width:18px;vertical-align:middle"><i class="fa fa-chevron-right text-muted" id="ic-' + j.id + '" style="font-size:10px"></i></td>'
        + '<td><strong>' + esc(j.destination_name || '&mdash;') + '</strong></td>'
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
    if (j.reason) html += '<p><strong>Reason:</strong> ' + esc(j.reason) + '</p>';
    if (j.assumption) html += '<p><strong>Assumption:</strong> <em class="text-warning">' + esc(j.assumption) + '</em></p>';
    if (j.error) html += '<div class="alert alert-danger" style="margin:0 0 8px"><i class="fa fa-warning"></i> ' + esc(j.error) + '</div>';
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

// ─── Batch ─────────────────────────────────────────────────────────────────
async function dryRun() {
    $('#btn-dry-run').prop('disabled', true);
    $('#batch-status').html('<i class="fa fa-spinner fa-spin"></i> Checking&hellip;');
    try {
        var res = await fetch('/batch/run', {
            method: 'POST',
            headers: {'Content-Type': 'application/json'},
            body: JSON.stringify({filter: {uncategorized_only: true}, dry_run: true})
        });
        if (res.status === 503) {
            $('#batch-status').html('<span class="text-warning"><i class="fa fa-warning"></i> Not configured &mdash; check Settings.</span>');
            return;
        }
        var d = await res.json();
        pendingDryRunCount = d.matching;
        $('#batch-status').html('<span class="text-muted">' + d.matching + ' uncategorized transaction(s) found.</span>');
        $('#btn-batch').prop('disabled', d.matching === 0);
    } catch (e) {
        $('#batch-status').html('<span class="text-danger">' + esc(e.message) + '</span>');
    } finally {
        $('#btn-dry-run').prop('disabled', false);
    }
}

function confirmBatch() {
    showModal('Run batch classification',
        'Classify ' + pendingDryRunCount + ' uncategorized transaction(s) using the configured AI provider? This action cannot be undone.',
        async function () {
            $('#btn-batch').prop('disabled', true);
            $('#batch-status').html('<i class="fa fa-spinner fa-spin"></i> Submitting&hellip;');
            var res = await fetch('/batch/run', {
                method: 'POST',
                headers: {'Content-Type': 'application/json'},
                body: JSON.stringify({filter: {uncategorized_only: true}})
            });
            var d = await res.json();
            $('#batch-status').html('<span class="text-muted">' + d.enqueued + ' job(s) enqueued.</span>');
        });
}

// ─── Transactions ──────────────────────────────────────────────────────────
function txnFlash(msg, type) {
    var el = document.getElementById('txn-flash');
    if (!msg) {el.innerHTML = ''; return;}
    el.innerHTML = '<div class="row"><div class="col-md-12"><div class="alert alert-' + type + '">' + msg + '</div></div></div>';
}

async function loadTransactions(page) {
    if (!page) page = 1;
    txnPage = page;
    var start = $('#txn-start').val(), end = $('#txn-end').val();
    var params = new URLSearchParams({page: page, limit: 50});
    if (start) params.set('start', start);
    if (end) params.set('end', end);
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
    // Show "select all pages" banner when all visible rows are selected and there are more pages
    if (master.checked && txnTotalPages > 1) {
        $('#txn-page-count').text(pageIds.length);
        $('#txn-all-count').text(txnTotal);
        $('#txn-select-all-bar').show();
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
    $('#txn-select-all-bar').hide();
    updateTxnSel();
}

function updateTxnSel() {
    var n = selectedTxns.size;
    $('#txn-sel-badge').toggle(n > 0).text(n + ' selected');
    $('#btn-recategorize').prop('disabled', n === 0);
}

async function recategorizeSelected() {
    var ids = Array.from(selectedTxns);
    if (!ids.length) return;
    showModal('Re-categorize transactions',
        'Run AI classification on ' + ids.length + ' selected transaction(s)? Existing categories will be replaced.',
        async function () {
            txnFlash('<i class="fa fa-spinner fa-spin"></i> Submitting ' + ids.length + ' job(s)&hellip;', 'info');
            var res = await fetch('/batch/run', {
                method: 'POST',
                headers: {'Content-Type': 'application/json'},
                body: JSON.stringify({filter: {transaction_ids: ids}, force: true})
            });
            if (!res.ok) {txnFlash('<i class="fa fa-times"></i> Error: ' + esc(await res.text()), 'danger'); return;}
            var d = await res.json();
            txnFlash('<i class="fa fa-check"></i> ' + d.enqueued + ' job(s) enqueued. Switch to Jobs to monitor progress.', 'success');
            selectedTxns.clear(); updateTxnSel();
        });
}

// Default date range: last 30 days
(function () {
    var end = new Date(), start = new Date();
    start.setDate(start.getDate() - 30);
    document.getElementById('txn-end').value = end.toISOString().slice(0, 10);
    document.getElementById('txn-start').value = start.toISOString().slice(0, 10);
})();

// ─── Categories ────────────────────────────────────────────────────────────
async function loadCategories() {
    var body = document.getElementById('cat-body');
    body.innerHTML = '<p><i class="fa fa-spinner fa-spin"></i> Loading&hellip;</p>';
    try {
        var res = await fetch('/api/categories');
        if (res.status === 503) {
            body.innerHTML = '<div class="alert alert-warning"><i class="fa fa-warning"></i> Firefly III is not configured. Set credentials in Settings.</div>';
            return;
        }
        if (!res.ok) throw new Error(await res.text());
        var cats = await res.json();
        if (!cats || !cats.length) {
            body.innerHTML = '<p class="text-muted">No categories found in Firefly III.</p>';
            return;
        }
        var html = '<div style="margin-bottom:12px">';
        cats.forEach(function (c) {
            html += '<span class="cat-chip"><i class="fa fa-bookmark-o" style="color:#3c8dbc;margin-right:5px"></i>'
                + esc(c.name)
                + (c.notes ? ' <span class="cat-notes">&mdash; ' + esc(c.notes) + '</span>' : '')
                + '</span>';
        });
        html += '</div><p class="text-muted"><small><i class="fa fa-info-circle"></i> '
            + cats.length + ' categories available to the AI.</small></p>';
        body.innerHTML = html;
    } catch (e) {
        body.innerHTML = '<div class="alert alert-danger"><i class="fa fa-times-circle"></i> ' + esc(e.message) + '</div>';
    }
}

// ─── Settings ──────────────────────────────────────────────────────────────
async function loadSettings() {
    settingsFlash('', '');
    try {
        var d = await (await fetch('/api/config')).json();
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
    $('#btn-test').prop('disabled', true);
    $('#test-result').html('<i class="fa fa-spinner fa-spin"></i> Testing&hellip;').removeClass('text-success text-danger').addClass('text-muted');
    try {
        var d = await (await fetch('/api/config/test')).json();
        if (d.ok) {
            $('#test-result').html('<i class="fa fa-check-circle"></i> Connected &mdash; ' + d.categories + ' categories found')
                .removeClass('text-muted text-danger').addClass('text-success');
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
    var ffUrl = $('#cfg-firefly-url').val().trim(), ffToken = $('#cfg-firefly-token').val();
    if (ffUrl) payload.firefly_url = ffUrl;
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
