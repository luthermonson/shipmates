export async function apiFetch(input, init) {
  const response = await fetch(input, init);
  if (response.status === 401 && window.location.pathname !== "/login") {
    window.location.assign("/login");
  }
  return response;
}
