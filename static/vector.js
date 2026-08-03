(() => {
  const app = document.querySelector('#vector-app');
  if (!app) return;
  const status = document.querySelector('#vector-status');
  const list = document.querySelector('#vector-project-list');
  const results = document.querySelector('#vector-results');
  const capacity = document.querySelector('#vector-capacity-list');
  const capacityPrice = document.querySelector('#vector-capacity-price');
  const reserveButton = document.querySelector('#vector-capacity-reserve');
  let projects = [];
  let capacityState = null;
  const escape = value => String(value).replace(/[&<>"']/g, ch => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[ch]));
  const api = async (url, options = {}) => {
    const response = await fetch(url, options);
    const data = await response.json().catch(() => ({error: 'Unreadable response'}));
    if (!response.ok) throw new Error(data.error || `Request failed (${response.status})`);
    return data;
  };
  const say = (message, bad = false) => { status.textContent = message; status.className = bad ? 'hot' : 'acid'; };
  function render() {
    list.innerHTML = projects.length ? projects.map(project => `<article class="panel">
      <p class="mono acid">${escape(project.slug)}</p><h3>${escape(project.name)}</h3>
      <p class="dim">${escape(project.tier)} · ${project.vector_count.toLocaleString()} vectors · ${escape(project.status)}</p>
      <p class="mono dim" style="font-size:12px">${escape(project.id)}</p></article>`).join('')
      : '<div class="panel dim">Create a project to index your first documents.</div>';
    document.querySelectorAll('.vector-project-select').forEach(select => {
      select.innerHTML = '<option value="">Choose project</option>' + projects.map(project =>
        `<option value="${escape(project.id)}">${escape(project.name)}</option>`).join('');
    });
  }
  async function refresh() { projects = (await api('/v1/vector/projects')).projects; render(); }
  function renderCapacity() {
    if (!capacityState) return;
    const plan = capacityState.plan;
    capacityPrice.textContent = `${plan.credits_per_hour.toLocaleString()} credits ($${plan.usd_per_hour.toFixed(2)}) per hour`;
    const open = capacityState.reservations.some(item => item.status === 'active' || item.status === 'paused');
    reserveButton.disabled = open || !capacityState.available;
    reserveButton.textContent = open ? 'Reserved' : (capacityState.available ? 'Reserve' : 'Currently full');
    capacity.innerHTML = capacityState.reservations.length ? capacityState.reservations.map(item => `<div class="row" style="margin-top:12px">
      <span><b class="${item.status === 'active' ? 'acid' : 'dim'}">${escape(item.cancelled_at && item.status === 'active' ? 'cancelling' : item.status)}</b> · ${item.cancelled_at && item.status === 'active' ? 'ends' : 'next renewal'} ${new Date(item.next_charge_at).toLocaleString()}${item.status_reason ? ` · ${escape(item.status_reason)}` : ''}</span>
      ${((item.status === 'active' || item.status === 'paused') && !item.cancelled_at) ? `<button class="btn ghost capacity-cancel" type="button" data-id="${escape(item.id)}">Cancel</button>` : ''}
    </div>`).join('') : '<span>No reservation yet. Shared search remains available.</span>';
  }
  async function refreshCapacity() {
    capacityState = await api('/v1/vector/reservations');
    renderCapacity();
  }
  document.querySelector('#vector-project-form').addEventListener('submit', async event => {
    event.preventDefault(); const form = new FormData(event.currentTarget);
    try { say('Creating project…'); await api('/v1/vector/projects', {method:'POST', headers:{'Content-Type':'application/json'}, body:JSON.stringify({name:form.get('name'),tier:form.get('tier')})}); event.currentTarget.reset(); await refresh(); say('Vector project created.'); }
    catch (error) { say(error.message, true); }
  });
  document.querySelector('#vector-upsert-form').addEventListener('submit', async event => {
    event.preventDefault(); const form = new FormData(event.currentTarget); const project = form.get('project_id');
    try { say('Embedding and indexing…'); const data = await api(`/v1/vector/projects/${encodeURIComponent(project)}/upsert`, {method:'POST', headers:{'Content-Type':'application/json'}, body:JSON.stringify({documents:[{id:form.get('document_id'),text:form.get('text'),metadata:{source:'dashboard'}}]})}); await refresh(); say(`Indexed ${data.upserted} document · ${data.vector_count} vectors total.`); }
    catch (error) { say(error.message, true); }
  });
  document.querySelector('#vector-search-form').addEventListener('submit', async event => {
    event.preventDefault(); const form = new FormData(event.currentTarget); const project = form.get('project_id');
    try { say('Searching…'); const data = await api(`/v1/vector/projects/${encodeURIComponent(project)}/search`, {method:'POST', headers:{'Content-Type':'application/json'}, body:JSON.stringify({query:form.get('query'),limit:10})}); results.innerHTML = `<p><b class="acid">${escape(data.backend)}</b> · <span class="mono">${data.latency_ms} ms</span></p>` + (data.matches.length ? `<table><thead><tr><th>ID</th><th>Score</th><th>Metadata</th></tr></thead><tbody>${data.matches.map(match => `<tr><td>${escape(match.id)}</td><td class="mono">${match.score}</td><td class="mono">${escape(JSON.stringify(match.metadata))}</td></tr>`).join('')}</tbody></table>` : '<p class="dim">No documents in this project yet.</p>'); say(`Search complete in ${data.latency_ms} ms.`); }
    catch (error) { say(error.message, true); }
  });
  reserveButton.addEventListener('click', async () => {
    try {
      reserveButton.disabled = true; say('Reserving capacity…');
      await api('/v1/vector/reservations', {method:'POST', headers:{'Content-Type':'application/json'}, body:JSON.stringify({product:'vector-performance'})});
      await refreshCapacity(); say('Capacity reserved. The first hour has been charged.');
    } catch (error) { say(error.message, true); await refreshCapacity().catch(() => {}); }
  });
  capacity.addEventListener('click', async event => {
    const button = event.target.closest('.capacity-cancel');
    if (!button) return;
    try {
      button.disabled = true; say('Cancelling reservation…');
      await api(`/v1/vector/reservations/${encodeURIComponent(button.dataset.id)}`, {method:'DELETE'});
      await refreshCapacity(); say('Cancellation scheduled. Capacity remains yours through the paid hour.');
    } catch (error) { say(error.message, true); }
  });
  Promise.all([refresh(), refreshCapacity()]).catch(error => say(error.message, true));
})();
