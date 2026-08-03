(() => {
  const app = document.querySelector('#data-app');
  if (!app) return;
  const status = document.querySelector('#data-status');
  const list = document.querySelector('#data-record-list');
  const recordForm = document.querySelector('#record-form');
  let databases = [];

  const escape = value => String(value).replace(/[&<>"']/g, ch => ({
    '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;'
  })[ch]);
  const api = async (url, options = {}) => {
    const response = await fetch(url, options);
    const data = await response.json().catch(() => ({error: 'Unreadable response'}));
    if (!response.ok) throw new Error(data.error || `Request failed (${response.status})`);
    return data;
  };
  const say = (message, bad = false) => {
    status.textContent = message;
    status.className = bad ? 'hot' : 'acid';
  };

  function renderDatabaseSelects() {
    document.querySelectorAll('.database-select').forEach(select => {
      const selected = select.value;
      select.innerHTML = '<option value="">Choose database</option>' + databases.map(database =>
        `<option value="${escape(database.id)}">${escape(database.name)} · ${database.records} records${database.vector_project_id ? ' · vector linked' : ''}</option>`
      ).join('');
      if (databases.some(database => database.id === selected)) select.value = selected;
      else if (databases.length) select.value = databases[0].id;
    });
  }

  async function refreshDatabases() {
    databases = (await api('/v1/data/namespaces')).databases;
    renderDatabaseSelects();
  }

  async function refreshRecords() {
    const databaseID = document.querySelector('#record-database').value;
    if (!databaseID) {
      list.innerHTML = '<div class="panel dim">Create or choose a database.</div>';
      return;
    }
    const data = await api(`/v1/data/namespaces/${encodeURIComponent(databaseID)}/records`);
    list.innerHTML = data.records.length ? data.records.map(record => `<article class="panel data-record">
      <p class="mono acid">${escape(record.key)}</p>
      <pre>${escape(JSON.stringify(record.value, null, 2))}</pre>
      ${record.search_text ? `<p class="dim">Indexed text: ${escape(record.search_text)}</p>` : '<p class="dim">Not vector indexed</p>'}
      <div class="data-record-actions">
        <button class="btn ghost data-edit" type="button" data-key="${escape(record.key)}">Edit</button>
        <button class="btn ghost data-delete" type="button" data-key="${escape(record.key)}">Delete</button>
      </div>
    </article>`).join('') : '<div class="panel dim">No records yet.</div>';
    list.querySelectorAll('.data-edit').forEach((button, index) => button.addEventListener('click', () => {
      const record = data.records[index];
      recordForm.elements.database_id.value = databaseID;
      recordForm.elements.key.value = record.key;
      recordForm.elements.value.value = JSON.stringify(record.value, null, 2);
      recordForm.elements.search_text.value = record.search_text || '';
      recordForm.elements.key.focus();
      recordForm.scrollIntoView({behavior: 'smooth', block: 'center'});
    }));
    list.querySelectorAll('.data-delete').forEach(button => button.addEventListener('click', async () => {
      const key = button.dataset.key;
      if (!window.confirm(`Delete ${key}? This cannot be undone.`)) return;
      try {
        say(`Deleting ${key}…`);
        await api(`/v1/data/namespaces/${encodeURIComponent(databaseID)}/records`, {
          method: 'DELETE', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({key})
        });
        await Promise.all([refreshDatabases(), refreshRecords()]);
        say(`Deleted ${key}.`);
      } catch (error) { say(error.message, true); }
    }));
  }

  document.querySelector('#database-form').addEventListener('submit', async event => {
    event.preventDefault();
    const form = new FormData(event.currentTarget);
    try {
      say('Creating database…');
      const data = await api('/v1/data/namespaces', {method: 'POST', headers: {'Content-Type': 'application/json'},
        body: JSON.stringify({name: form.get('name'), vector_project_id: form.get('vector_project_id') || ''})});
      event.currentTarget.reset();
      await refreshDatabases();
      document.querySelectorAll('.database-select').forEach(select => { select.value = data.database.id; });
      await refreshRecords();
      say('Database created.');
    } catch (error) { say(error.message, true); }
  });

  recordForm.addEventListener('submit', async event => {
    event.preventDefault();
    const form = new FormData(event.currentTarget);
    let value;
    try { value = JSON.parse(form.get('value')); }
    catch { say('JSON value is not valid.', true); return; }
    const databaseID = form.get('database_id');
    try {
      say('Saving record…');
      const data = await api(`/v1/data/namespaces/${encodeURIComponent(databaseID)}/records`, {
        method: 'PUT', headers: {'Content-Type': 'application/json'},
        body: JSON.stringify({key: form.get('key'), value, search_text: form.get('search_text') || ''})
      });
      document.querySelector('#record-database').value = databaseID;
      await Promise.all([refreshDatabases(), refreshRecords()]);
      say(data.vector.status === 'ready' ? 'Record saved and vector indexed.' : `Record saved · vector ${data.vector.status}.`);
    } catch (error) { say(error.message, true); }
  });

  document.querySelector('#record-database').addEventListener('change', () => refreshRecords().catch(error => say(error.message, true)));
  document.querySelector('#refresh-records').addEventListener('click', () => refreshRecords().catch(error => say(error.message, true)));

  Promise.all([refreshDatabases(), api('/v1/vector/projects').catch(() => ({projects: []}))]).then(([, vectors]) => {
    const select = document.querySelector('#database-vector-project');
    select.innerHTML = '<option value="">Not linked</option>' + vectors.projects.map(project =>
      `<option value="${escape(project.id)}">${escape(project.name)}</option>`).join('');
    return refreshRecords();
  }).catch(error => say(error.message, true));
})();
