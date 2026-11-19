// The managed instance's companion page (GitHub #587,
// operations-api-contract-design.md §7.2.3a): create, list and revoke the
// owner's companion keys through the existing routes. The key is shown once,
// in this page's memory only, and never written into the page's HTML.
(function () {
    // Every value from the API reaches the page through textContent, never
    // as markup.
    var keysURL = '/api/v1/companion/keys?projectId=companion';
    var grantURL = '/api/v1/companion/grant';
    var revokeBase = '/api/v1/projects/companion/keys/';
    var body = document.getElementById('companion-keys');
    var errBox = document.getElementById('companion-error');

    function showError(msg) {
        errBox.textContent = msg;
        errBox.classList.remove('hidden');
    }
    function cell(text) {
        var td = document.createElement('td');
        td.className = 'py-1 pr-2';
        td.textContent = text || '';
        return td;
    }
    // grants says in words what a key may do (GitHub #846).
    function grants(k) {
        var can = [];
        if (k.memoryWrite) { can.push('read and write memory'); } else if (k.memoryRead) { can.push('read memory'); }
        if (k.skillAdmin) { can.push('approve skills'); }
        if (k.skillWrite) { can.push('propose skills'); } else if (k.skillRead) { can.push('read skills'); }
        if (!k.delegateDisabled) { can.push(k.budgetCapUsd ? 'start work (budget $' + k.budgetCapUsd + ')' : 'start work'); }
        return can.length ? can.join(', ') : 'nothing';
    }
    function day() { return new Date().toISOString().slice(0, 10); }
    async function load() {
        var res = await fetch(keysURL, { credentials: 'same-origin', headers: { 'Accept': 'application/json' } });
        if (!res.ok) { showError('The keys could not be listed (' + res.status + ').'); return; }
        var data = await res.json();
        var keys = Array.isArray(data.keys) ? data.keys : [];
        body.replaceChildren();
        keys.filter(function (k) { return !k.revokedAt; }).forEach(function (k) {
            var tr = document.createElement('tr');
            tr.appendChild(cell(k.sessionLabel || k.keyPrefix));
            tr.appendChild(cell(k.clientKind));
            tr.appendChild(cell(grants(k)));
            tr.appendChild(cell(k.createdAt));
            tr.appendChild(cell(k.lastUsedAt || 'never'));
            var td = document.createElement('td');
            var btn = document.createElement('button');
            btn.type = 'button';
            btn.className = 'text-xs text-red-400';
            btn.textContent = 'Revoke';
            btn.addEventListener('click', async function () {
                if (!confirm('Revoke this key? A companion using it stops working.')) { return; }
                var r = await fetch(revokeBase + encodeURIComponent(k.id), { method: 'DELETE', credentials: 'same-origin' });
                if (!r.ok) { showError('The key could not be revoked (' + r.status + ').'); return; }
                load();
            });
            td.appendChild(btn);
            tr.appendChild(td);
            body.appendChild(tr);
        });
    }
    async function createKey(grant) {
        var res = await fetch(grantURL, {
            method: 'POST',
            credentials: 'same-origin',
            headers: { 'Content-Type': 'application/json', 'Accept': 'application/json' },
            body: JSON.stringify(grant)
        });
        if (!res.ok) { showError('The key could not be created (' + res.status + ').'); return; }
        var data = await res.json();
        document.getElementById('companion-key').textContent = data.secret || '';
        document.getElementById('companion-key-box').classList.remove('hidden');
        load();
    }
    document.getElementById('companion-create').addEventListener('click', function () {
        createKey({
            projectId: 'companion', clientKind: 'claude-code', sessionLabel: 'web ' + day(),
            memoryRead: true, memoryWrite: true, skillRead: true, skillWrite: true,
            budgetCapUsd: 25
        });
    });
    // A read-only key for AWS DevOps Agent (GitHub #846, design 26 §3 C7).
    document.getElementById('companion-create-aws').addEventListener('click', function () {
        createKey({
            projectId: 'companion', clientKind: 'aws-devops-agent', sessionLabel: 'aws-devops-agent ' + day(),
            memoryRead: true, skillRead: true, delegateDisabled: true
        });
    });
    load();
})();
