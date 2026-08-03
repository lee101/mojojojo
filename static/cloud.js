(() => {
  const app = document.querySelector('#cloud-app');
  if (!app) return;
  const status = document.querySelector('#cloud-status');
  const projectList = document.querySelector('#project-list');
  const assetList = document.querySelector('#asset-list');
  let projects = [];

  const api = async (url, options = {}) => {
    const response = await fetch(url, options);
    const data = await response.json().catch(() => ({ error: 'Unreadable response' }));
    if (!response.ok) throw new Error(data.error || `Request failed (${response.status})`);
    return data;
  };
  const bytes = value => value < 1024 * 1024
    ? `${(value / 1024).toFixed(1)} KB`
    : `${(value / 1024 / 1024).toFixed(1)} MB`;
  const escape = value => String(value).replace(/[&<>"']/g, ch => ({
    '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;'
  })[ch]);
  const say = (message, bad = false) => {
    status.textContent = message;
    status.className = bad ? 'hot' : 'acid';
  };

  function renderProjects() {
    projectList.innerHTML = projects.length ? projects.map(project => `
      <article class="panel">
        <p class="mono acid">${escape(project.slug)}</p>
        <h3>${escape(project.name)}</h3>
        <p class="dim">${escape(project.runtime)} · ${project.active_deployment_id ? 'deployed' : 'not deployed'}</p>
        ${project.url ? `<a href="${escape(project.url)}" target="_blank" rel="noopener">Open deployment →</a>` : ''}
      </article>`).join('') : '<div class="panel dim">Create a project to deploy your first bundle.</div>';
    document.querySelectorAll('.project-select').forEach(select => {
      const optional = select.closest('#asset-form');
      select.innerHTML = (optional ? '<option value="">Account storage</option>' : '<option value="">Choose project</option>') +
        projects.map(project => `<option value="${escape(project.id)}">${escape(project.name)}</option>`).join('');
    });
  }

  async function refresh() {
    const [projectData, storage] = await Promise.all([api('/v1/projects'), api('/v1/storage')]);
    projects = projectData.projects;
    renderProjects();
    document.querySelector('#storage-used').textContent = `${bytes(storage.used_bytes)} used · ${(storage.included_bytes / 1073741824).toFixed(0)} GB included`;
    document.querySelector('#storage-price').textContent = `$${storage.gb_month_usd.toFixed(2)}/GB-month after included storage · estimated $${storage.estimated_monthly_usd.toFixed(2)}/month`;
    assetList.innerHTML = storage.assets.length ? `<table><thead><tr><th>Asset</th><th>Size</th><th>URL</th></tr></thead><tbody>${storage.assets.map(asset =>
      `<tr><td>${escape(asset.filename)}</td><td class="mono">${bytes(asset.bytes)}</td><td><a href="${escape(asset.url)}" target="_blank" rel="noopener">open</a></td></tr>`
    ).join('')}</tbody></table>` : 'No assets yet.';
  }

  document.querySelector('#project-form').addEventListener('submit', async event => {
    event.preventDefault();
    const form = new FormData(event.currentTarget);
    try {
      say('Creating project…');
      await api('/v1/projects', { method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ name: form.get('name'), runtime: form.get('runtime') }) });
      event.currentTarget.reset();
      await refresh();
      say('Project created.');
    } catch (error) { say(error.message, true); }
  });

  document.querySelector('#deploy-form').addEventListener('submit', async event => {
    event.preventDefault();
    const form = new FormData(event.currentTarget);
    try {
      say('Uploading and validating deployment…');
      const data = await api('/v1/deployments', { method: 'POST', body: form });
      await refresh();
      say(`Deployed ${data.deployment.file_count} files: ${data.deployment.url}`);
    } catch (error) { say(error.message, true); }
  });

  document.querySelector('#asset-form').addEventListener('submit', async event => {
    event.preventDefault();
    const form = new FormData(event.currentTarget);
    const file = form.get('asset');
    try {
      say('Signing upload…');
      const signed = await api('/v1/uploads/sign', { method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ filename: file.name, content_type: file.type || 'application/octet-stream',
          size_bytes: file.size, project_id: form.get('project_id') || '' }) });
      say('Uploading asset…');
      await api(signed.upload_url, { method: 'PUT', headers: { 'Content-Type': file.type || 'application/octet-stream' }, body: file });
      event.currentTarget.reset();
      await refresh();
      say(`Uploaded: ${signed.asset_url}`);
    } catch (error) { say(error.message, true); }
  });

  refresh().catch(error => say(error.message, true));
})();
