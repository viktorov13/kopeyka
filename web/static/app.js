'use strict';

(() => {
  const $ = (id) => document.getElementById(id);
  const state = { config: null, users: [], items: [], total: 0, offset: 0, limit: 20, request: 0, hasLoadedExpenses: false, editID: null, deleteID: null, loading: false, saving: false, deleting: false };
  const moneyFormat = new Intl.NumberFormat('ru-RU', { minimumFractionDigits: 2, maximumFractionDigits: 2 });
  const integerFormat = new Intl.NumberFormat('ru-RU');
  let toastTimer;
  let searchTimer;
  const icons = {
    food: '<path d="M5 3v6c0 2 4 2 4 0V3M7 3v18M16 3v18M16 3c5 3 5 9 0 9"/>',
    transport: '<rect x="4" y="5" width="16" height="12" rx="3"/><path d="M4 11h16M7 17v3m10-3v3M8 14h.01M16 14h.01M8 5l1-2h6l1 2"/>',
    home: '<path d="m3 10 9-7 9 7M5 9v12h14V9M9 21v-8h6v8"/>',
    health: '<path d="M9 3h6v6h6v6h-6v6H9v-6H3V9h6z"/>',
    fun: '<path d="m12 3 2.8 5.7 6.2.9-4.5 4.4 1.1 6.2L12 17.3l-5.6 2.9 1.1-6.2L3 9.6l6.2-.9z"/>',
    shopping: '<path d="M4 7h16l-1 14H5L4 7zM8 7V6a4 4 0 0 1 8 0v1"/>',
    other: '<rect x="4" y="4" width="16" height="16" rx="4"/><path d="M8 12h.01M12 12h.01M16 12h.01"/>',
    edit: '<path d="m14 5 5 5M4 20l5-1L20 8a2 2 0 0 0-5-5L4 14z"/>',
    trash: '<path d="M3 6h18M9 6V3h6v3M5 6l1 15h12l1-15M10 10v7m4-7v7"/>'
  };
  const categoryStyles = [
    { match: /еда|продукт|кафе|ресторан|питан/i, icon: 'food', color: '#9fb45c', bg: '#f1f5e5' },
    { match: /транспорт|такси|авто/i, icon: 'transport', color: '#90a9b6', bg: '#edf3f6' },
    { match: /дом|жиль|коммун/i, icon: 'home', color: '#b59b7a', bg: '#f6f1e9' },
    { match: /здоров|медиц|аптек/i, icon: 'health', color: '#b292a8', bg: '#f5eef2' },
    { match: /развлеч|досуг|спорт|образ/i, icon: 'fun', color: '#a39bbb', bg: '#f1eef7' },
    { match: /покуп|одежд/i, icon: 'shopping', color: '#c2a372', bg: '#f8f3e9' }
  ];
  const genericCategory = { icon: 'other', color: '#a7b096', bg: '#f1f3ed' };
  const categoryStyle = (category) => categoryStyles.find((entry) => entry.match.test(category)) || genericCategory;
  const svg = (name) => `<svg viewBox="0 0 24 24" aria-hidden="true">${icons[name] || icons.other}</svg>`;
  const escapeHTML = (value) => String(value ?? '').replace(/[&<>"']/g, (character) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[character]));
  const money = (value) => `${moneyFormat.format(Number(value) || 0)} ₽`;
  const summaryMoney = (value) => `${moneyFormat.format(Number(value) || 0)} <span class="currency">₽</span>`;
  const plural = (count, words = ['расход', 'расхода', 'расходов']) => words[count % 10 === 1 && count % 100 !== 11 ? 0 : count % 10 >= 2 && count % 10 <= 4 && (count % 100 < 12 || count % 100 > 14) ? 1 : 2];
  const displayDate = (value) => {
    if (!value) return '—';
    const date = new Date(`${value.slice(0, 10)}T12:00:00`);
    return Number.isNaN(date.getTime()) ? value : date.toLocaleDateString('ru-RU', { day: 'numeric', month: 'short', year: 'numeric' }).replace(' г.', '');
  };

  async function api(path, options = {}) {
    let response;
    try {
      response = await fetch(path, { ...options, headers: { Accept: 'application/json', ...(options.body ? { 'Content-Type': 'application/json' } : {}), ...options.headers } });
    } catch {
      throw new Error('Не удалось связаться с сервером. Проверьте подключение и попробуйте ещё раз.');
    }
    if (response.status === 204) return null;
    let data;
    try { data = await response.json(); } catch { throw new Error('Сервер вернул неожиданный ответ. Попробуйте ещё раз.'); }
    if (!response.ok) throw new Error(data.error || 'Не удалось выполнить запрос. Попробуйте ещё раз.');
    return data;
  }

  function showToast(message) {
    clearTimeout(toastTimer);
    $('toast').textContent = message;
    $('toast').hidden = false;
    toastTimer = setTimeout(() => { $('toast').hidden = true; }, 4200);
  }

  function filters() {
    return { search: $('filter-search').value.trim(), user_id: $('filter-user').value, category: $('filter-category').value, from: $('filter-from').value, to: $('filter-to').value };
  }

  function hasFilters() { return Object.values(filters()).some(Boolean); }

  function setPeriodLabel() {
    const { from, to } = filters();
    $('period-label').textContent = from && to ? `${displayDate(from)} — ${displayDate(to)}` : from ? `С ${displayDate(from)}` : to ? `По ${displayDate(to)}` : 'За всё время';
  }

  function renderSummary(data) {
    state.hasLoadedExpenses = true;
    const total = Number(data.total) || 0;
    $('total-amount').innerHTML = summaryMoney(data.total_amount);
    $('expense-count').textContent = integerFormat.format(total);
    $('average-amount').innerHTML = summaryMoney(total ? Number(data.total_amount) / total : 0);
    $('ledger-count').textContent = integerFormat.format(total);
    setPeriodLabel();
    const entries = [...(data.category_totals || [])].sort((a, b) => Number(b.amount) - Number(a.amount));
    const totalAmount = Number(data.total_amount) || 0;
    $('category-breakdown').innerHTML = entries.length ? entries.map((entry) => {
      const style = categoryStyle(entry.category);
      const percentage = totalAmount > 0 ? Math.max(0, Math.min(100, Number(entry.amount) / totalAmount * 100)) : 0;
      const count = Number(entry.count) || 0;
      return `<div class="category-stat" style="--category-color:${style.color}"><div class="category-stat-header"><span class="category-stat-name">${escapeHTML(entry.category)}</span><strong>${money(entry.amount)}</strong></div><div class="category-progress" aria-label="${escapeHTML(entry.category)}: ${Math.round(percentage)}%"><span style="width:${percentage}%"></span></div><div class="category-stat-footer"><span>${integerFormat.format(count)} ${plural(count)}</span><span>${percentage.toLocaleString('ru-RU', { maximumFractionDigits: 1 })}%</span></div></div>`;
    }).join('') : '<p class="muted">Здесь появится распределение по категориям, когда вы добавите расходы.</p>';
  }

  function showUnavailableSummary(message) {
    if (state.hasLoadedExpenses) return;
    ['total-amount', 'expense-count', 'average-amount', 'ledger-count'].forEach((id) => { $(id).textContent = '—'; });
    $('category-breakdown').textContent = message;
  }

  function renderRows() {
    $('expense-rows').innerHTML = state.items.map((expense) => {
      const style = categoryStyle(expense.category);
      const title = expense.comment || expense.category;
      return `<tr><td><div class="expense-details"><span class="category-icon" style="--category-bg:${style.bg};--category-color:${style.color}">${svg(style.icon)}</span><div class="expense-description"><strong title="${escapeHTML(title)}">${escapeHTML(title)}</strong><small>${escapeHTML(expense.category)}</small></div></div></td><td><span class="owner-tag" title="Telegram ID">${escapeHTML(expense.user_id)}</span></td><td class="date-cell">${escapeHTML(displayDate(expense.spent_at))}</td><td class="amount-cell expense-amount">${money(expense.amount)}</td><td><div class="row-actions"><button type="button" class="icon-button edit-action" data-edit="${escapeHTML(expense.id)}" aria-label="Изменить расход: ${escapeHTML(title)}">${svg('edit')}</button><button type="button" class="icon-button delete-action" data-delete="${escapeHTML(expense.id)}" aria-label="Удалить расход: ${escapeHTML(title)}">${svg('trash')}</button></div></td></tr>`;
    }).join('');
    $('table-state').hidden = state.items.length > 0;
    if (!state.items.length) {
      const filtered = hasFilters();
      $('table-state').innerHTML = `<span class="empty-symbol" aria-hidden="true">${filtered ? '⌕' : '＋'}</span><h3>${filtered ? 'По этим фильтрам ничего не найдено' : 'Первая запись — начало порядка'}</h3><p>${filtered ? 'Попробуйте другой период, участника или категорию.' : 'Добавьте расход здесь или отправьте его Telegram-боту. Все траты появятся в этой таблице.'}</p><button type="button" class="button button-secondary" data-empty-action="${filtered ? 'reset' : 'add'}">${filtered ? 'Сбросить фильтры' : 'Добавить первый расход'}</button>`;
    }
    const page = Math.floor(state.offset / state.limit) + 1;
    const pages = Math.max(1, Math.ceil(state.total / state.limit));
    $('page-label').textContent = `${page} / ${pages}`;
    $('pagination-label').textContent = state.total ? `${state.offset + 1}–${Math.min(state.offset + state.items.length, state.total)} из ${integerFormat.format(state.total)} ${plural(state.total)}` : 'Пока нет расходов';
    $('prev-page').disabled = state.offset === 0;
    $('next-page').disabled = state.offset + state.limit >= state.total;
  }

  async function loadExpenses() {
    const request = ++state.request;
    const values = filters();
    if (values.from && values.to && values.from > values.to) {
      state.loading = false;
      $('table-container').setAttribute('aria-busy', 'false');
      $('overview').setAttribute('aria-busy', 'false');
      $('prev-page').disabled = true;
      $('next-page').disabled = true;
      $('page-error-message').textContent = 'Начальная дата не может быть позже конечной.';
      $('page-error').hidden = false;
      $('pagination-label').textContent = 'Проверьте выбранный период';
      if (!state.items.length) {
        $('table-state').hidden = false;
        $('table-state').innerHTML = '<span class="empty-symbol" aria-hidden="true">!</span><h3>Проверьте выбранный период</h3><p>Начальная дата не может быть позже конечной.</p>';
      }
      showUnavailableSummary('Выберите корректный период, чтобы увидеть расходы по категориям.');
      return;
    }
    state.loading = true;
    $('page-error').hidden = true;
    $('table-container').setAttribute('aria-busy', 'true');
    $('overview').setAttribute('aria-busy', 'true');
    $('prev-page').disabled = true;
    $('next-page').disabled = true;
    if (!state.items.length) {
      $('table-state').hidden = false;
      $('table-state').innerHTML = '<span class="loading-spinner" aria-hidden="true"></span><h3>Загружаем расходы</h3><p>Собираем всё в одном месте.</p>';
    }
    const query = new URLSearchParams({ limit: state.limit, offset: state.offset });
    for (const [key, value] of Object.entries(values)) if (value) query.set(key, value);
    try {
      const data = await api(`/api/expenses?${query}`);
      if (request !== state.request) return;
      state.items = data.items || [];
      state.total = Number(data.total) || 0;
      if (state.offset > 0 && state.offset >= state.total) {
        state.offset = Math.max(0, Math.floor((state.total - 1) / state.limit) * state.limit);
        await loadExpenses();
        return;
      }
      renderSummary(data);
      renderRows();
    } catch (error) {
      if (request !== state.request) return;
      $('page-error-message').textContent = error.message;
      $('page-error').hidden = false;
      showUnavailableSummary('Не удалось загрузить расходы по категориям. Нажмите «Повторить».');
      if (!state.items.length) {
        $('table-state').innerHTML = '<span class="empty-symbol" aria-hidden="true">!</span><h3>Не удалось загрузить расходы</h3><p>Проверьте подключение и нажмите «Повторить».</p>';
        $('pagination-label').textContent = 'Данные недоступны';
      } else {
        $('pagination-label').textContent = 'Показаны ранее загруженные данные';
      }
    } finally {
      if (request === state.request) {
        state.loading = false;
        $('table-container').setAttribute('aria-busy', 'false');
        $('overview').setAttribute('aria-busy', 'false');
      }
    }
  }

  async function loadUsers() {
    const data = await api('/api/users');
    state.users = data.items || [];
    const selected = $('filter-user').value;
    $('filter-user').innerHTML = '<option value="">Все участники</option>' + state.users.map((user) => `<option value="${escapeHTML(user.user_id)}">ID ${escapeHTML(user.user_id)}</option>`).join('');
    if (selected && !state.users.some((user) => String(user.user_id) === selected)) {
      $('filter-user').insertAdjacentHTML('beforeend', `<option value="${escapeHTML(selected)}">ID ${escapeHTML(selected)}</option>`);
    }
    $('filter-user').value = selected;
    $('known-users').innerHTML = state.users.map((user) => `<option value="${escapeHTML(user.user_id)}">${integerFormat.format(Number(user.count) || 0)} ${plural(Number(user.count) || 0)}</option>`).join('');
  }

  async function refreshAfterMutation() {
    const results = await Promise.allSettled([loadExpenses(), loadUsers()]);
    if (results[1].status === 'rejected') showToast('Изменения сохранены. Список участников обновится после перезагрузки.');
  }

  async function reloadData() {
    const results = await Promise.allSettled([loadUsers(), loadExpenses()]);
    if (results[0].status === 'rejected') {
      $('page-error-message').textContent = 'Не удалось загрузить список участников. Попробуйте ещё раз.';
      $('page-error').hidden = false;
    }
  }

  function changeFilters() {
    clearTimeout(searchTimer);
    state.offset = 0;
    loadExpenses();
  }

  function markPeriod(period) {
    document.querySelectorAll('[data-period]').forEach((button) => {
      const active = button.dataset.period === period;
      button.classList.toggle('selected', active);
      button.setAttribute('aria-pressed', String(active));
    });
  }

  function resetFilters() {
    $('filter-form').reset();
    markPeriod('all');
    changeFilters();
  }

  function openExpense(expense = null) {
    if (!state.config) { showToast('Дождитесь подключения к серверу.'); return; }
    state.editID = expense ? expense.id : null;
    $('expense-form').reset();
    $('expense-error').hidden = true;
    $('expense-dialog-title').textContent = expense ? 'Изменить расход' : 'Новый расход';
    $('save-expense').textContent = expense ? 'Сохранить изменения' : 'Добавить расход';
    $('expense-amount').value = expense ? expense.amount.replace('.', ',') : '';
    $('expense-category').value = expense ? expense.category : state.config.categories[0] || '';
    $('expense-date').value = expense ? expense.spent_at.slice(0, 10) : state.config.today;
    $('expense-user').value = expense ? expense.user_id : $('filter-user').value || (state.users.length === 1 ? state.users[0].user_id : '');
    $('expense-comment').value = expense ? expense.comment : '';
    $('expense-dialog').showModal();
    $('expense-amount').focus();
  }

  async function saveExpense(event) {
    event.preventDefault();
    if (state.saving) return;
    $('expense-error').hidden = true;
    const amount = $('expense-amount').value.trim().replace(/\s/g, '').replace(',', '.');
    const userID = Number($('expense-user').value.trim());
    if (!/^\d+(\.\d{1,2})?$/.test(amount) || Number(amount) <= 0) {
      $('expense-error').textContent = 'Введите сумму больше нуля с точностью до двух знаков после запятой.';
      $('expense-error').hidden = false;
      $('expense-amount').focus();
      return;
    }
    if (!Number.isSafeInteger(userID) || userID <= 0) {
      $('expense-error').textContent = 'Введите положительный числовой Telegram ID участника.';
      $('expense-error').hidden = false;
      $('expense-user').focus();
      return;
    }
    const payload = { amount, user_id: userID, category: $('expense-category').value, spent_at: $('expense-date').value, comment: $('expense-comment').value.trim() };
    const editing = state.editID !== null;
    state.saving = true;
    $('save-expense').disabled = true;
    $('save-expense').textContent = 'Сохраняем…';
    try {
      await api(editing ? `/api/expenses/${encodeURIComponent(state.editID)}` : '/api/expenses', { method: editing ? 'PATCH' : 'POST', body: JSON.stringify(payload) });
      $('expense-dialog').close();
      showToast(editing ? 'Изменения сохранены' : 'Расход добавлен');
      state.offset = 0;
      await refreshAfterMutation();
    } catch (error) {
      $('expense-error').textContent = error.message;
      $('expense-error').hidden = false;
    } finally {
      state.saving = false;
      $('save-expense').disabled = false;
      $('save-expense').textContent = editing ? 'Сохранить изменения' : 'Добавить расход';
    }
  }

  function openDelete(expense) {
    state.deleteID = expense.id;
    $('delete-description').textContent = `${expense.comment || expense.category} · ${money(expense.amount)} · ${displayDate(expense.spent_at)}`;
    $('delete-error').hidden = true;
    $('delete-dialog').showModal();
  }

  async function deleteExpense() {
    if (state.deleting) return;
    state.deleting = true;
    $('confirm-delete').disabled = true;
    $('confirm-delete').textContent = 'Удаляем…';
    $('delete-error').hidden = true;
    try {
      await api(`/api/expenses/${encodeURIComponent(state.deleteID)}`, { method: 'DELETE' });
      $('delete-dialog').close();
      showToast('Расход удалён');
      await refreshAfterMutation();
    } catch (error) {
      $('delete-error').textContent = error.message;
      $('delete-error').hidden = false;
    } finally {
      state.deleting = false;
      $('confirm-delete').disabled = false;
      $('confirm-delete').textContent = 'Удалить';
    }
  }

  async function initialize() {
    $('page-error').hidden = true;
    try {
      state.config = await api('/api/config');
      const options = state.config.categories.map((category) => `<option value="${escapeHTML(category)}">${escapeHTML(category)}</option>`).join('');
      $('filter-category').innerHTML = '<option value="">Все категории</option>' + options;
      $('expense-category').innerHTML = options;
      const llmMode = state.config.mode === 'llm';
      $('bot-status-title').textContent = state.config.bot_enabled ? 'Telegram-бот настроен' : 'Telegram-бот не подключён';
      $('bot-status-text').textContent = state.config.bot_enabled ? (llmMode ? 'ИИ-помощник по расходам' : 'Демо · разбор по правилам') : 'Добавьте токен в настройках сервера';
      $('bot-status-dot').classList.toggle('connected', Boolean(state.config.bot_enabled));
      $('bot-mode-title').textContent = llmMode ? 'ИИ-помощник' : 'Демонстрационный режим';
      $('bot-mode-text').textContent = llmMode
        ? 'Бот понимает обычные сообщения с помощью языковой модели. Если данных не хватает, он задаёт вопрос. Напишите «отмена», чтобы начать новый разговор. Если модель недоступна, бот сообщит об ошибке, сохранив расходы без изменений.'
        : 'Бот разбирает ограниченный набор фраз по правилам, без языковой модели. Примеры выше поддерживаются; «помощь» покажет все форматы, «отмена» сбросит уточнение. ИИ-режим можно включить в настройках сервера.';
      $('timezone-label').textContent = `Рубли · ${state.config.timezone}`;
      await reloadData();
    } catch (error) {
      $('page-error-message').textContent = error.message;
      $('page-error').hidden = false;
      $('bot-status-title').textContent = 'Сервер недоступен';
      $('bot-status-text').textContent = 'Проверьте подключение';
      $('bot-mode-title').textContent = 'Режим бота недоступен';
      $('bot-mode-text').textContent = 'Не удалось получить настройки. Повторите подключение к серверу.';
      $('table-container').setAttribute('aria-busy', 'false');
      $('overview').setAttribute('aria-busy', 'false');
      showUnavailableSummary('Данные недоступны. Нажмите «Повторить», когда сервер будет доступен.');
      $('table-state').innerHTML = '<span class="empty-symbol" aria-hidden="true">!</span><h3>Не удалось подключиться</h3><p>Нажмите «Повторить», когда сервер будет доступен.</p>';
      $('pagination-label').textContent = 'Данные недоступны';
    }
  }

  $('filter-form').addEventListener('submit', (event) => { event.preventDefault(); changeFilters(); });
  $('filter-search').addEventListener('input', () => { clearTimeout(searchTimer); searchTimer = setTimeout(changeFilters, 300); });
  ['filter-user', 'filter-category'].forEach((id) => $(id).addEventListener('change', changeFilters));
  ['filter-from', 'filter-to'].forEach((id) => $(id).addEventListener('change', () => { markPeriod(''); changeFilters(); }));
  $('reset-filters').addEventListener('click', resetFilters);
  $('add-expense').addEventListener('click', () => openExpense());
  $('help-button').addEventListener('click', () => $('help-dialog').showModal());
  $('expense-form').addEventListener('submit', saveExpense);
  $('confirm-delete').addEventListener('click', deleteExpense);
  $('retry-load').addEventListener('click', () => state.config ? reloadData() : initialize());
  $('prev-page').addEventListener('click', () => { if (state.loading) return; state.offset = Math.max(0, state.offset - state.limit); loadExpenses(); });
  $('next-page').addEventListener('click', () => { if (state.loading) return; state.offset += state.limit; loadExpenses(); });
  document.querySelectorAll('[data-close]').forEach((button) => button.addEventListener('click', () => {
    if ((button.dataset.close === 'expense-dialog' && state.saving) || (button.dataset.close === 'delete-dialog' && state.deleting)) return;
    $(button.dataset.close).close();
  }));
  $('expense-dialog').addEventListener('cancel', (event) => { if (state.saving) event.preventDefault(); });
  $('delete-dialog').addEventListener('cancel', (event) => { if (state.deleting) event.preventDefault(); });
  $('expense-rows').addEventListener('click', (event) => {
    const button = event.target.closest('button[data-edit],button[data-delete]');
    if (!button) return;
    const expense = state.items.find((item) => String(item.id) === String(button.dataset.edit || button.dataset.delete));
    if (!expense) return;
    button.dataset.edit ? openExpense(expense) : openDelete(expense);
  });
  $('table-state').addEventListener('click', (event) => {
    const button = event.target.closest('[data-empty-action]');
    if (button) button.dataset.emptyAction === 'reset' ? resetFilters() : openExpense();
  });
  document.querySelectorAll('[data-period]').forEach((button) => button.addEventListener('click', () => {
    if (!state.config) return;
    const period = button.dataset.period;
    const today = state.config.today;
    let from = '';
    let to = '';
    if (period === 'month') {
      from = `${today.slice(0, 7)}-01`;
      const date = new Date(`${today}T12:00:00Z`);
      date.setUTCMonth(date.getUTCMonth() + 1, 0);
      to = date.toISOString().slice(0, 10);
    } else if (period === 'week') {
      const date = new Date(`${today}T12:00:00Z`);
      date.setUTCDate(date.getUTCDate() - 6);
      from = date.toISOString().slice(0, 10);
      to = today;
    }
    $('filter-from').value = from;
    $('filter-to').value = to;
    markPeriod(period);
    changeFilters();
  }));
  markPeriod('all');
  initialize();
})();
